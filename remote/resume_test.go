package remote_test

import (
	"context"
	"errors"
	"net"
	"strings"
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

// A lost connection costs nothing: another client attaches from the last
// offset it read and gets exactly what followed, and the shell kept its
// state.
func TestLostConnectionThenResumeContinuesExactly(t *testing.T) {
	t.Parallel()
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(1)})
	if err != nil {
		t.Fatal(err)
	}
	dial := serveDialer(t, inner)
	firstClient := dial()
	firstEnv, err := firstClient.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := running(t, firstEnv)
	sh := execenvtest.StartShell(t, firstEnv, fence, "sh")
	if err := sh.Type("KEPT=across"); err != nil {
		t.Fatal(err)
	}
	if err := sh.Type(`echo one""1`); err != nil {
		t.Fatal(err)
	}
	sh.Expect("one1")
	mark := sh.Offset()
	_ = firstClient.Close()
	if _, err := sh.Attachment().Next(t.Context()); !errors.Is(err, execenv.ErrConnection) {
		t.Fatalf("Next() after a lost connection = %v, want ErrConnection", err)
	}

	secondEnv, err := dial().Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := secondEnv.Process(t.Context(), "sh")
	if err != nil || !info.Running {
		t.Fatalf("Process() after a lost connection = %+v %v", info, err)
	}
	a, err := secondEnv.AttachProcess(t.Context(), "sh", map[execenv.Stream]uint64{execenv.StreamPTY: mark})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := secondEnv.WriteInput(t.Context(), fence, "sh", info.Input, []byte("echo $KEPT-x\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var seen strings.Builder
	for !strings.Contains(seen.String(), "across-x") {
		ev, err := a.Next(ctx)
		if err != nil {
			t.Fatalf("resumed Next() = %v (saw %q)", err, seen.String())
		}
		if ev.Kind != execenv.OutputData {
			continue
		}
		if ev.Offset != mark+uint64(seen.Len()) {
			t.Fatalf("resumed at %d, want %d", ev.Offset, mark+uint64(seen.Len()))
		}
		seen.Write(ev.Data)
	}
	if strings.Contains(seen.String(), "one1") {
		t.Fatalf("resume replayed output the first client had read: %q", seen.String())
	}
}
