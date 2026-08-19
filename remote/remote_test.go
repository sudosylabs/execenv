package remote_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

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
	obs, err := env.Watch(t.Context())
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
