package remote_test

import (
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
	execenvtest.Run(t, func(t *testing.T, cfg execenvtest.Config) execenv.Host {
		t.Helper()
		inner, err := memory.New(memory.Config{
			Images:   []execenv.Image{"default", "alt"},
			Capacity: execenv.DefaultGrant.Times(2),
		})
		if err != nil {
			t.Fatal(err)
		}
		return serveInner(t, inner)
	})
}

// Loss crosses the wire: every call on a lost grant, and its process
// attachments, fail with ErrLost on the client.
func TestLossConformance(t *testing.T) {
	execenvtest.RunLoss(t, func(t *testing.T) execenvtest.Lossy {
		t.Helper()
		inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(2)})
		if err != nil {
			t.Fatal(err)
		}
		return execenvtest.Lossy{Host: serveInner(t, inner), Lose: func(t *testing.T, id execenv.ID) {
			if !inner.Lose(id) {
				t.Fatalf("Lose(%s) found no live grant", id)
			}
		}}
	})
}

func TestDrainingCrossesTheWire(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(2)})
	if err != nil {
		t.Fatal(err)
	}
	host := serveInner(t, inner)
	held, err := host.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	inner.SetDraining(true)
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usable || report.Reason != execenv.ReasonDraining || report.Grants != 1 {
		t.Fatalf("Ready() while draining = %+v", report)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "new", Image: "default"}); !errors.Is(err, execenv.ErrDraining) {
		t.Fatalf("Ensure() while draining error = %v, want ErrDraining", err)
	}
	again, err := host.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"})
	if err != nil || again.Epoch() != held.Epoch() {
		t.Fatalf("Ensure() of a held grant while draining = %v", err)
	}
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

func TestDifferentGrantsDoNotBlockEachOther(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(2)})
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
	go func() { _, err := first.ControlStatus(context.Background()); errCh <- err }()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := second.ControlStatus(ctx); err != nil {
		t.Fatalf("second grant blocked behind first: %v", err)
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestObservationsReplayAcrossRemoteReconnect(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(1)})
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
	fence := execenv.ProjectionFence{ExecutionGrantID: firstEnv.ID(), EnvironmentEpoch: firstEnv.Epoch(), FenceRevision: 1}
	if _, err := firstEnv.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := firstEnv.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init"}); err != nil {
		t.Fatal(err)
	}
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
	page, err := secondEnv.Observe(t.Context(), fence, 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Observations) != 1 || page.Observations[0].Path != "offline.txt" || page.Observations[0].Operation != execenv.ObservationCreateFile {
		t.Fatalf("replayed observations = %+v", page.Observations)
	}
	reader, err := secondEnv.OpenObservationContent(t.Context(), fence, *page.Observations[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(body) != "kept" {
		t.Fatalf("replayed content = %q %v", body, err)
	}
}

func TestClosingAnAttachmentLeavesTheProcess(t *testing.T) {
	t.Parallel()
	host := startLoopback(t)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := running(t, env)
	sh := execenvtest.StartShell(t, env, fence, "sh")
	if err := sh.Attachment().Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := env.ControlStatus(t.Context())
	if err != nil || receipt.State != execenv.ControlRunning {
		t.Fatalf("ControlStatus() after detaching = %+v %v", receipt, err)
	}
	info, err := env.Process(t.Context(), "sh")
	if err != nil || !info.Running {
		t.Fatalf("Process() after detaching = %+v %v", info, err)
	}
}

func running(t *testing.T, env execenv.Env) execenv.ProjectionFence {
	t.Helper()
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	return fence
}

func startLoopback(t *testing.T) execenv.Host {
	t.Helper()
	inner, err := memory.New(memory.Config{
		Images:   []execenv.Image{"default", "alt"},
		Capacity: execenv.DefaultGrant.Times(2),
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

func (e blockingEnv) ControlStatus(ctx context.Context) (execenv.ControlReceipt, error) {
	e.once.Do(func() { close(e.started) })
	select {
	case <-ctx.Done():
		return execenv.ControlReceipt{}, ctx.Err()
	case <-e.release:
		return e.Env.ControlStatus(ctx)
	}
}
