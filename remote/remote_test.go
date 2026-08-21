package remote_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
	"github.com/sudosylabs/execenv/remote"
)

func TestConformance(t *testing.T) {
	execenvtest.Run(t, func(t *testing.T) execenv.Host {
		t.Helper()
		return startLoopback(t)
	})
}

func TestProductionRefusesCleartext(t *testing.T) {
	t.Parallel()
	_, err := remote.New(remote.Config{
		Address:  "127.0.0.1:1",
		Security: remote.SecurityTLS,
		Token:    []byte("secret"),
	})
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("New() error = %v, want ErrInvalid", err)
	}
}

func TestInsecureRefusesNonLoopback(t *testing.T) {
	t.Parallel()
	_, err := remote.New(remote.Config{
		Address:  "8.8.8.8:8443",
		Security: remote.SecurityInsecureLocal,
		Token:    []byte("secret"),
	})
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("New() error = %v, want ErrInvalid", err)
	}
}

func TestReadyReportsRelease(t *testing.T) {
	host := startLoopback(t)
	rep, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Release != execenv.Release {
		t.Fatalf("Ready() Release = %q, want %q", rep.Release, execenv.Release)
	}
}

func TestOperationTimeoutCancelsHostCall(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = remote.Serve(ctx, ln, slowReadyHost{Host: inner}, remote.ServerConfig{
			Security:         remote.SecurityInsecureLocal,
			Token:            []byte("secret"),
			OperationTimeout: time.Second,
		})
	}()
	client, err := remote.New(remote.Config{
		Address:          ln.Addr().String(),
		Security:         remote.SecurityInsecureLocal,
		Token:            []byte("secret"),
		OperationTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	started := time.Now()
	if _, err := client.Ready(t.Context()); !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Ready() error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Ready() took %v, operation timeout was not enforced", elapsed)
	}
}

func TestAttachEchoesBytes(t *testing.T) {
	t.Parallel()
	host := startLoopback(t)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(term, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ping" {
		t.Fatal("remote PTY did not echo the inner host")
	}
}

func TestAttachKeepsOutputProducedBeforeReply(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	client := serveInner(t, promptHost{Host: inner})
	env, err := client.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	got := make([]byte, len("prompt"))
	if _, err := io.ReadFull(term, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "prompt" {
		t.Fatalf("initial output = %q", got)
	}
}

func TestDifferentGrantsDoNotBlockEachOther(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	client := serveInner(t, &blockingHost{Host: inner, started: started, release: release})
	first, err := client.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Ensure(t.Context(), execenv.Spec{ID: "grant-2", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- first.ReplaceTree(context.Background(), nil) }()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := second.Apply(ctx, execenv.Batch{}); err != nil {
		t.Fatalf("second grant blocked behind first: %v", err)
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestWatchReplaysAcrossRemoteReconnect(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = remote.Serve(ctx, ln, inner, remote.ServerConfig{Security: remote.SecurityInsecureLocal, Token: []byte("secret")})
	}()
	dial := func() *remote.Client {
		client, err := remote.New(remote.Config{Address: ln.Addr().String(), Security: remote.SecurityInsecureLocal, Token: []byte("secret")})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	firstClient := dial()
	firstEnv, err := firstClient.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	firstObs, err := firstEnv.Watch(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := firstObs.Cursor()
	if err := firstClient.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inner.WriteGuest(t.Context(), "grant-1", "offline.txt", []byte("kept")); err != nil {
		t.Fatal(err)
	}
	secondClient := dial()
	t.Cleanup(func() { _ = secondClient.Close() })
	secondEnv, err := secondClient.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := secondEnv.Watch(t.Context(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	event, err := resumed.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if event.Path != "offline.txt" || event.Cursor == "" {
		t.Fatalf("replayed event = %+v", event)
	}
}

func TestDroppedPtyIsHangupNotRevoke(t *testing.T) {
	t.Parallel()
	host := startLoopback(t)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	// Control and tree must still work on the same grant.
	if err := env.ReplaceTree(t.Context(), execenv.Tree{{
		Path:    "still.txt",
		Kind:    execenv.KindFile,
		Version: "v1",
		Data:    []byte("ok"),
	}}); err != nil {
		t.Fatalf("ReplaceTree() after PTY close error = %v", err)
	}
	again, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatalf("Attach() after hangup error = %v", err)
	}
	_ = again.Close()
}

func TestRemoteWatchHarvestsInnerGuestWrite(t *testing.T) {
	t.Parallel()
	inner, err := memory.New(memory.Config{
		Images: []execenv.Image{"default", "alt"},
		Slots:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := serveInner(t, inner)
	env, err := client.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := env.Watch(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	if err := inner.WriteGuest(t.Context(), "grant-1", "out.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	ev, err := obs.Next(t.Context())
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if ev.Path != "out.txt" {
		t.Fatalf("event path = %q", ev.Path)
	}
	body, err := env.Open(t.Context(), "out.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi" {
		t.Fatal("Open() returned unexpected bytes")
	}
}

func startLoopback(t *testing.T) execenv.Host {
	t.Helper()
	inner, err := memory.New(memory.Config{
		Images: []execenv.Image{"default", "alt"},
		Slots:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return serveInner(t, inner)
}

func serveInner(t *testing.T, inner execenv.Host) execenv.Host {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = remote.Serve(ctx, ln, inner, remote.ServerConfig{
			Security: remote.SecurityInsecureLocal,
			Token:    []byte("secret"),
		})
	}()
	client, err := remote.New(remote.Config{
		Address:  ln.Addr().String(),
		Security: remote.SecurityInsecureLocal,
		Token:    []byte("secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type promptHost struct {
	execenv.Host
}

func (h promptHost) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	env, err := h.Host.Ensure(ctx, spec)
	if err != nil {
		return nil, err
	}
	return promptEnv{Env: env}, nil
}

type promptEnv struct {
	execenv.Env
}

func (e promptEnv) Attach(ctx context.Context, window execenv.Window) (execenv.Terminal, error) {
	term, err := e.Env.Attach(ctx, window)
	if err != nil {
		return nil, err
	}
	return &promptTerminal{Terminal: term, prompt: bytes.NewReader([]byte("prompt"))}, nil
}

type promptTerminal struct {
	execenv.Terminal
	prompt *bytes.Reader
}

func (t *promptTerminal) Read(p []byte) (int, error) {
	if t.prompt.Len() > 0 {
		return t.prompt.Read(p)
	}
	return t.Terminal.Read(p)
}

type blockingHost struct {
	execenv.Host
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type slowReadyHost struct {
	execenv.Host
}

func (h slowReadyHost) Ready(ctx context.Context) (execenv.Report, error) {
	<-ctx.Done()
	return execenv.Report{}, ctx.Err()
}

func (h *blockingHost) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	env, err := h.Host.Ensure(ctx, spec)
	if err != nil {
		return nil, err
	}
	if spec.ID != "grant-1" {
		return env, nil
	}
	return blockingEnv{Env: env, started: h.started, release: h.release, once: &h.once}, nil
}

type blockingEnv struct {
	execenv.Env
	started chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (e blockingEnv) ReplaceTree(ctx context.Context, tree execenv.Tree) error {
	e.once.Do(func() { close(e.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.release:
		return e.Env.ReplaceTree(ctx, tree)
	}
}
