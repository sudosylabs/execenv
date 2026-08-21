package remote

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func TestAuthRefusesReleaseMismatch(t *testing.T) {
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
		_ = Serve(ctx, ln, inner, ServerConfig{
			Security: SecurityInsecureLocal,
			Token:    []byte("secret"),
			claim:    "host-stamp",
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
	}
	_, err = New(Config{
		Address:  ln.Addr().String(),
		Security: SecurityInsecureLocal,
		Token:    []byte("secret"),
	})
	if !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("New() error = %v, want ErrUnavailable", err)
	}
}

func TestAuthTimeoutClosesSilentConnection(t *testing.T) {
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
		_ = Serve(ctx, ln, inner, ServerConfig{
			Security:    SecurityInsecureLocal,
			Token:       []byte("secret"),
			AuthTimeout: 50 * time.Millisecond,
		})
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := conn.Read(one[:]); err == nil {
		t.Fatal("silent unauthenticated connection remained open")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("client deadline expired before the server auth timeout closed the connection")
	}
}
