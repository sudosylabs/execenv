package execenvtest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest/internal/portfixture"
)

// PortListener starts a fixture on address in the given grant. Stop closes
// the listener before it returns. The test also owns cleanup.
type PortListener func(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, address, mode string) (port int, stop func())

type portRegistrar interface {
	RegisterPortListener(execenv.Env, *net.TCPListener) error
}

// LocalPortListener starts real listeners and registers them when the
// adapter needs explicit registration.
func LocalPortListener(registrar portRegistrar) PortListener {
	return func(t *testing.T, env execenv.Env, _ execenv.ProjectionFence, address, mode string) (int, func()) {
		t.Helper()
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP(address)})
		if err != nil {
			t.Fatal(err)
		}
		stop := func() { _ = listener.Close() }
		t.Cleanup(stop)
		if registrar != nil {
			if err := registrar.RegisterPortListener(env, listener); err != nil {
				t.Fatal(err)
			}
		}
		go portfixture.Serve(listener, mode)
		return listener.Addr().(*net.TCPAddr).Port, stop
	}
}

// GuestPortListener builds the static listener used by live Linux guests.
func GuestPortListener(t *testing.T) PortListener {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "portfixture")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", exe, "github.com/sudosylabs/execenv/execenvtest/internal/portfixture/cmd")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build portfixture: %v\n%s", err, out)
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	return func(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, address, mode string) (int, func()) {
		t.Helper()
		sequence++
		tag := fmt.Sprintf("portfixture-%d", sequence)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if _, err := env.WriteFile(ctx, fence, tag, bytes.NewReader(binary), execenv.WriteOptions{Executable: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.Start(ctx, fence, execenv.ProcessSpec{Tag: tag, Argv: []string{execenv.GuestWorkspace + "/" + tag, address, mode}}); err != nil {
			t.Fatal(err)
		}
		a, err := env.AttachProcess(ctx, tag, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		var output strings.Builder
		for {
			event, err := a.Next(ctx)
			if err != nil || event.Kind == execenv.OutputExit {
				t.Fatalf("portfixture exited or failed: %v, %s", err, output.String())
			}
			if event.Kind == execenv.OutputData && event.Stream == execenv.StreamStdout {
				output.Write(event.Data)
				if line, _, ok := strings.Cut(output.String(), "\n"); ok {
					port, err := strconv.Atoi(line)
					if err != nil {
						t.Fatal(err)
					}
					return port, func() {
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						defer cancel()
						if err := env.SignalProcess(ctx, fence, tag, execenv.SignalKill); err != nil {
							t.Fatal(err)
						}
						if _, _, err := drain(ctx, a); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
	}
}

// RunPorts tests port connections on a grant with NetworkNone.
func RunPorts(t *testing.T, host execenv.Host, listen PortListener) {
	t.Helper()
	env, fence := occupyRunning(t, host, "ports")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	open := func(t *testing.T, port int) execenv.PortConn {
		t.Helper()
		conn, err := env.ConnectPort(ctx, fence, port)
		if err != nil {
			t.Fatal(err)
		}
		if conn.Port() != port {
			t.Fatalf("Port() = %d, want %d", conn.Port(), port)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	t.Run("HTTP on concurrent connections", func(t *testing.T) {
		port, _ := listen(t, env, fence, "127.0.0.1", "http")
		a, b := open(t, port), open(t, port)
		for _, conn := range []execenv.PortConn{a, b} {
			if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
		}
		for _, conn := range []execenv.PortConn{a, b} {
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(body) != "hello" || resp.StatusCode != http.StatusOK {
				t.Fatalf("response = %d %q, %v", resp.StatusCode, body, err)
			}
		}
	})
	t.Run("half close in both directions", func(t *testing.T) {
		port, _ := listen(t, env, fence, "127.0.0.1", "echo")
		conn := open(t, port)
		if _, err := io.WriteString(conn, "echo after EOF"); err != nil {
			t.Fatal(err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(conn)
		if err != nil || string(body) != "echo after EOF" {
			t.Fatalf("ReadAll() = %q, %v", body, err)
		}

		port, _ = listen(t, env, fence, "127.0.0.1", "send-fin")
		conn = open(t, port)
		body, err = io.ReadAll(conn)
		if err != nil || string(body) != "ready" {
			t.Fatalf("ReadAll() = %q, %v", body, err)
		}
		if _, err := io.WriteString(conn, "write after EOF"); err != nil {
			t.Fatal(err)
		}
		if err := conn.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("refused invalid and stale fence", func(t *testing.T) {
		port, stop := listen(t, env, fence, "127.0.0.1", "hold")
		stale := fence
		stale.FenceRevision++
		if _, err := env.ConnectPort(ctx, stale, port); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("ConnectPort(stale) = %v, want ErrConflict", err)
		}
		stop()
		if _, err := env.ConnectPort(ctx, fence, port); !errors.Is(err, execenv.ErrRefused) {
			t.Fatalf("ConnectPort(closed listener) = %v, want ErrRefused", err)
		}
		for _, invalid := range []int{0, 65536} {
			if _, err := env.ConnectPort(ctx, fence, invalid); !errors.Is(err, execenv.ErrInvalid) {
				t.Fatalf("ConnectPort(%d) = %v, want ErrInvalid", invalid, err)
			}
		}
	})
	t.Run("freeze and revoke end every connection", func(t *testing.T) {
		port, _ := listen(t, env, fence, "127.0.0.1", "hold")
		testEnd := func(want error, end func() error) {
			t.Helper()
			a, b := open(t, port), open(t, port)
			if err := end(); err != nil {
				t.Fatal(err)
			}
			for _, conn := range []execenv.PortConn{a, b} {
				if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, want) {
					t.Fatalf("Read() = %v, want %v", err, want)
				}
				if _, err := conn.Write([]byte("x")); !errors.Is(err, want) {
					t.Fatalf("Write() = %v, want %v", err, want)
				}
			}
		}
		testEnd(execenv.ErrFrozen, func() error {
			fence.FenceRevision++
			_, err := env.Control(ctx, execenv.ControlRequest{Fence: fence, State: execenv.ControlFrozen})
			return err
		})
		fence.FenceRevision++
		if _, err := env.Control(ctx, execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		testEnd(execenv.ErrRevoked, func() error { return env.Revoke(ctx) })
	})
}
