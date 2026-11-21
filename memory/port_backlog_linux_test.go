//go:build linux

package memory_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func TestRevokeWhilePortDialBlocked(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "backlog", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Revoke(context.Background())
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	raw, err := l.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var listenErr error
	if err := raw.Control(func(fd uintptr) { listenErr = unix.Listen(int(fd), 0) }); err != nil {
		t.Fatal(err)
	}
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	dialer := net.Dialer{Timeout: time.Second}
	filler, err := dialer.DialContext(t.Context(), "tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer filler.Close()
	if err := host.RegisterPortListener(env, l); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		c, err := env.ConnectPort(t.Context(), fence, l.Addr().(*net.TCPAddr).Port)
		if c != nil {
			_ = c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("dial returned with a full accept queue: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := env.Revoke(ctx); err != nil {
		t.Fatalf("revoke during dial: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("blocked dial returned %v, want ErrRevoked", err)
		}
	case <-ctx.Done():
		t.Fatal("revoke did not interrupt the blocked dial")
	}
}
