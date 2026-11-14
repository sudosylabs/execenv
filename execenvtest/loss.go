package execenvtest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// Lossy is a host whose grants' environments a test can end without a
// revoke, as a machine that died or a guest that crashed would.
type Lossy struct {
	// Host must advertise image "default" and accept two grants.
	Host execenv.Host
	// Lose ends the environment of the occupied grant id without a revoke.
	// It may return before the host has noticed.
	Lose func(t *testing.T, id execenv.ID)
}

// LossFactory returns a fresh Lossy for one subtest.
type LossFactory func(t *testing.T) Lossy

// lossNotice bounds how long a host may take to notice a lost environment.
const lossNotice = 15 * time.Second

// RunLoss exercises what a caller sees when a grant's environment ends
// without a revoke. It needs a way to end one, so it is separate from Run.
func RunLoss(t *testing.T, factory LossFactory) {
	t.Helper()

	t.Run("a lost grant fails every call with ErrLost", func(t *testing.T) {
		l := factory(t)
		env, fence := occupyRunning(t, l.Host, "lost-grant")
		term, err := env.Attach(t.Context(), execenv.AttachRequest{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		l.Lose(t, env.ID())
		waitLost(t, env)

		// Output produced before the loss may still drain; the stream then
		// ends with ErrLost, never a clean end of file.
		readDone := make(chan error, 1)
		go func() {
			_, err := io.Copy(io.Discard, term)
			if err == nil {
				err = io.EOF
			}
			readDone <- err
		}()
		select {
		case err := <-readDone:
			if !errors.Is(err, execenv.ErrLost) {
				t.Fatalf("terminal Read error = %v, want ErrLost", err)
			}
		case <-time.After(lossNotice):
			t.Fatal("terminal Read did not end after the loss")
		}
		if _, err := term.Write([]byte("echo late\n")); !errors.Is(err, execenv.ErrLost) {
			t.Fatalf("terminal Write error = %v, want ErrLost", err)
		}
		if _, err := env.Attach(t.Context(), execenv.AttachRequest{}); !errors.Is(err, execenv.ErrLost) {
			t.Fatalf("Attach() error = %v, want ErrLost", err)
		}
		next := fence
		next.FenceRevision++
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: next, State: execenv.ControlFrozen}); !errors.Is(err, execenv.ErrLost) {
			t.Fatalf("Control() error = %v, want ErrLost", err)
		}
		if _, err := env.ControlStatus(t.Context()); !errors.Is(err, execenv.ErrLost) {
			t.Fatalf("ControlStatus() error = %v, want ErrLost", err)
		}
		if _, err := env.Observe(t.Context(), fence, 0, 1); !errors.Is(err, execenv.ErrLost) {
			t.Fatalf("Observe() error = %v, want ErrLost", err)
		}
		if _, err := env.ObserveProcesses(t.Context(), fence, 0, 1); !errors.Is(err, execenv.ErrLost) {
			t.Fatalf("ObserveProcesses() error = %v, want ErrLost", err)
		}
		if _, err := env.UploadProjectionContent(t.Context(), fence, strings.NewReader("late")); !errors.Is(err, execenv.ErrLost) {
			t.Fatalf("UploadProjectionContent() error = %v, want ErrLost", err)
		}
		if err := env.Revoke(t.Context()); err != nil {
			t.Fatalf("Revoke() of a lost grant error = %v, want nil", err)
		}
	})

	t.Run("a lost grant frees its slot and stays lost until revoked", func(t *testing.T) {
		l := factory(t)
		before, err := l.Host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		spec := execenv.Spec{ID: "lost-slot", Image: "default"}
		env, _ := occupyRunning(t, l.Host, spec.ID)
		held, err := l.Host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if held.Slots != before.Slots-1 || held.Grants != before.Grants+1 {
			t.Fatalf("Ready() with a grant = %+v, before %+v", held, before)
		}
		l.Lose(t, spec.ID)
		waitLost(t, env)
		after, err := l.Host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.Slots != before.Slots || after.Grants != before.Grants || after.Lost != before.Lost+1 {
			t.Fatalf("Ready() after the loss = %+v, before %+v", after, before)
		}
		if _, err := l.Host.Ensure(t.Context(), spec); !errors.Is(err, execenv.ErrLost) {
			t.Fatalf("Ensure() of a lost grant error = %v, want ErrLost", err)
		}
		other, err := l.Host.Ensure(t.Context(), execenv.Spec{ID: "lost-other", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() of another grant error = %v", err)
		}
		t.Cleanup(func() { _ = l.Host.Revoke(context.Background(), other.ID()) })
		if err := l.Host.Revoke(t.Context(), spec.ID); err != nil {
			t.Fatalf("Revoke() error = %v", err)
		}
		fresh, err := l.Host.Ensure(t.Context(), spec)
		if err != nil {
			t.Fatalf("Ensure() after Revoke error = %v", err)
		}
		t.Cleanup(func() { _ = l.Host.Revoke(context.Background(), spec.ID) })
		if fresh.Epoch() == env.Epoch() {
			t.Fatal("Ensure() after a loss reused the lost epoch")
		}
		if _, err := fresh.ControlStatus(t.Context()); err != nil {
			t.Fatalf("fresh ControlStatus() error = %v", err)
		}
	})
}

func occupyRunning(t *testing.T, host execenv.Host, id execenv.ID) (execenv.Env, execenv.ProjectionFence) {
	t.Helper()
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: id, Image: "default"})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	t.Cleanup(func() { _ = env.Revoke(context.Background()) })
	fence := execenv.ProjectionFence{ExecutionGrantID: id, EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatalf("Control(running) error = %v", err)
	}
	return env, fence
}

// waitLost waits until the host reports env lost.
func waitLost(t *testing.T, env execenv.Env) {
	t.Helper()
	deadline := time.Now().Add(lossNotice)
	for {
		_, err := env.ControlStatus(t.Context())
		if errors.Is(err, execenv.ErrLost) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ControlStatus() error = %v, want ErrLost after the loss", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
