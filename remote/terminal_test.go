package remote_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
	"github.com/sudosylabs/execenv/remote"
)

// serveDialer serves inner and returns a function that opens a new client
// connection to it.
func serveDialer(t *testing.T, inner execenv.Host) func() *remote.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = remote.Serve(ctx, ln, inner, remote.ServerConfig{Security: remote.SecurityInsecureLocal, Token: []byte("secret")})
	}()
	return func() *remote.Client {
		client, err := remote.New(remote.Config{Address: ln.Addr().String(), Security: remote.SecurityInsecureLocal, Token: []byte("secret")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
}

// attachWhenDetached retries while the host has not yet seen the previous
// client go away.
func attachWhenDetached(t *testing.T, env execenv.Env, request execenv.AttachRequest) execenv.Terminal {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		term, err := env.Attach(t.Context(), request)
		if err == nil {
			t.Cleanup(func() { _ = term.Close() })
			return term
		}
		if !errors.Is(err, execenv.ErrBusy) || time.Now().After(deadline) {
			t.Fatalf("Attach(%+v) error = %v", request, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLostConnectionThenResumeContinuesExactly(t *testing.T) {
	t.Parallel()
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	dial := serveDialer(t, inner)
	firstClient := dial()
	firstEnv, err := firstClient.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstEnv.Attach(t.Context(), execenv.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out := execenvtest.Watch(t, first)
	if _, err := first.Write([]byte("KEPT=across\necho one\"\"1\n")); err != nil {
		t.Fatal(err)
	}
	out.WaitFor("one1")
	_ = firstClient.Close()
	seen, readErr := out.Stop()
	if !errors.Is(readErr, execenv.ErrConnection) {
		t.Fatalf("Read after lost connection = %v, want ErrConnection", readErr)
	}
	if first.Offset() != first.Start()+uint64(len(seen)) {
		t.Fatalf("Offset = %d after %d bytes", first.Offset(), len(seen))
	}
	at := bytes.Index(seen, []byte("one1"))
	position := first.Start() + uint64(at)

	secondEnv, err := dial().Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	second := attachWhenDetached(t, secondEnv, execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: first.ID(), Offset: position}})
	if second.ID() != first.ID() || second.Start() != position {
		t.Fatalf("resume ID=%q Start=%d, want %q at %d", second.ID(), second.Start(), first.ID(), position)
	}
	replay := make([]byte, len(seen)-at)
	if _, err := io.ReadFull(second, replay); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replay, seen[at:]) {
		t.Fatalf("replay = %q, want %q", replay, seen[at:])
	}
	rest := execenvtest.Watch(t, second)
	if _, err := second.Write([]byte("echo $KEPT-x\n")); err != nil {
		t.Fatal(err)
	}
	rest.WaitFor("across-x")
}

func TestSlowRemoteReaderLagsThenResumes(t *testing.T) {
	t.Parallel()
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	dial := serveDialer(t, inner)
	spec := execenv.Spec{ID: "grant-1", Image: "default"}
	env, err := dial().Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	// Produce more than the client may buffer before reading any of it.
	// Each burst fits the host's history, so the host never skips it; the
	// client sees every byte and must overflow.
	lagged := false
	for range 200 {
		if _, err := term.Write([]byte("seq 1 40000\n")); err != nil {
			if !errors.Is(err, execenv.ErrLagged) {
				t.Fatalf("Write() error = %v", err)
			}
			lagged = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !lagged {
		t.Fatal("client buffered without bound")
	}
	// The overfull client detaches itself; another connection sees the
	// session free once it has.
	probeEnv, err := dial().Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	probe := attachWhenDetached(t, probeEnv, execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: term.ID()}})
	_ = probe.Close()
	read, err := io.Copy(io.Discard, term)
	if !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("Read of an overfull client = %v after %d bytes, want ErrLagged", err, read)
	}
	// Offset may jump past Start+read: the host skips output a slow reader lost.
	if read > 1<<20 || term.Offset() < term.Start()+uint64(read) {
		t.Fatalf("drained %d bytes, Offset %d", read, term.Offset())
	}
	resumed := attachWhenDetached(t, env, execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: term.ID(), Offset: term.Offset()}})
	if resumed.ID() != term.ID() || resumed.Start() < term.Offset() {
		t.Fatalf("resume ID=%q Start=%d after Offset %d", resumed.ID(), resumed.Start(), term.Offset())
	}
	out := execenvtest.Watch(t, resumed)
	if _, err := resumed.Write([]byte("echo resumed\"\"ok\n")); err != nil {
		t.Fatal(err)
	}
	out.WaitFor("resumedok")
}

func TestRemoteHangupEndsSession(t *testing.T) {
	t.Parallel()
	host := startLoopback(t)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Hangup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Read(make([]byte, 1)); !errors.Is(err, execenv.ErrClosed) {
		t.Fatalf("Read after Hangup = %v, want ErrClosed", err)
	}
	_, err = env.Attach(t.Context(), execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: term.ID()}})
	if !errors.Is(err, execenv.ErrNotFound) {
		t.Fatalf("resume after Hangup = %v, want ErrNotFound", err)
	}
}
