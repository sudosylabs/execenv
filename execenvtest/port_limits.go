package execenvtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// RunPortLimits verifies the per-grant connection cap and live bandwidth
// changes while using an environment with NetworkNone.
func RunPortLimits(t *testing.T, host execenv.Host, listen PortListener) {
	t.Helper()
	const rate = 128 << 10
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	env, err := host.Ensure(ctx, execenv.Spec{
		ID:    "port-limits",
		Image: "default",
		Limits: execenv.Limits{
			PortBytes: execenv.TokenBucket{Rate: rate},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Revoke(context.Background()) })
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(ctx, execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	open := func(t *testing.T, port int) execenv.PortConn {
		t.Helper()
		conn, err := env.ConnectPort(ctx, fence, port)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	t.Run("connection cap", func(t *testing.T) {
		first, _ := listen(t, env, fence, "127.0.0.1", "hold")
		second, _ := listen(t, env, fence, "127.0.0.1", "hold")
		var conns []execenv.PortConn
		for range execenv.MaxPortConns {
			conns = append(conns, open(t, first))
		}
		capped, err := env.ConnectPort(ctx, fence, second)
		if err == nil {
			_ = capped.Close()
		}
		if !errors.Is(err, execenv.ErrBusy) {
			t.Fatalf("ConnectPort after %d opens = %v, want ErrBusy", len(conns), err)
		}
		if err := conns[0].Close(); err != nil {
			t.Fatal(err)
		}

		// The remote server observes the client's stream cancellation
		// asynchronously. Allow time for it to release the slot.
		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			replacement, err := env.ConnectPort(deadline, fence, second)
			if err == nil {
				_ = replacement.Close()
				break
			}
			if !errors.Is(err, execenv.ErrBusy) {
				t.Fatalf("ConnectPort after Close() = %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Run("bandwidth in both directions", func(t *testing.T) {
		port, _ := listen(t, env, fence, "127.0.0.1", "echo")
		conn := open(t, port)
		payload := bytes.Repeat([]byte("0123456789ABCDEF"), (96<<10)/16)
		start := time.Now()
		if err := portEcho(conn, payload); err != nil {
			t.Fatal(err)
		}
		// The guest waits for EOF before responding. At this rate the
		// two legs take about 1.5s, or about 750ms if only one is limited.
		if elapsed := time.Since(start); elapsed < 1100*time.Millisecond {
			t.Fatalf("round trip took %s; the port bucket should slow both legs", elapsed)
		}
	})
	t.Run("update wakes an existing stream", func(t *testing.T) {
		port, _ := listen(t, env, fence, "127.0.0.1", "echo")
		network := execenv.TokenBucket{Rate: 1}
		if err := env.UpdateLimits(ctx, fence, execenv.Limits{NetworkBytes: network, PortBytes: execenv.TokenBucket{Rate: 1}}); err != nil {
			t.Fatal(err)
		}
		conn := open(t, port)
		done := make(chan error, 1)
		go func() { done <- portEcho(conn, make([]byte, 4096)) }()
		select {
		case err := <-done:
			t.Fatalf("transfer ended before the rate change: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		if err := env.UpdateLimits(ctx, fence, execenv.Limits{NetworkBytes: network}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("transfer remained throttled after setting a zero port rate")
		}
	})
	t.Run("network budget does not throttle ports", func(t *testing.T) {
		if err := env.UpdateLimits(ctx, fence, execenv.Limits{NetworkBytes: execenv.TokenBucket{Rate: 1}}); err != nil {
			t.Fatal(err)
		}
		port, _ := listen(t, env, fence, "127.0.0.1", "echo")
		fast, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		conn, err := env.ConnectPort(fast, fence, port)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := portEcho(conn, make([]byte, 64<<10)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unread ports leave process and file traffic responsive", func(t *testing.T) {
		port, _ := listen(t, env, fence, "127.0.0.1", "flood")
		deadline := time.Now().Add(5 * time.Second)
		for i := 0; i < execenv.MaxPortConns; {
			conn, err := env.ConnectPort(ctx, fence, port)
			if errors.Is(err, execenv.ErrBusy) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if err != nil {
				t.Fatalf("open unread port connection %d: %v", i, err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			i++
		}
		sh := StartShell(t, env, fence, "port-pressure")
		if err := sh.Type(`echo port""s-responsive`); err != nil {
			t.Fatal(err)
		}
		sh.Expect("ports-responsive")
		fast, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := env.WriteFile(fast, fence, "port-test.txt", bytes.NewBufferString("file responsive"), execenv.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		r, err := env.ReadFile(fast, "port-test.txt", 0)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if body, err := io.ReadAll(r); err != nil || string(body) != "file responsive" {
			t.Fatalf("read under port pressure = %q, %v", body, err)
		}
	})

}

func portEcho(conn execenv.PortConn, payload []byte) error {
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	if err := conn.CloseWrite(); err != nil {
		return err
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, payload) {
		return fmt.Errorf("echo changed the payload: read %d bytes, wrote %d", len(body), len(payload))
	}
	return nil
}
