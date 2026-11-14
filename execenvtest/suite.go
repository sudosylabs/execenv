// Package execenvtest contains a reusable Host conformance suite.
//
// Run covers occupancy, terminal sessions, fenced control, and the journal
// projection. Guest-originated observations need a real or simulated guest:
// RunIgnore covers IgnoreRules given a Guest that acts inside the
// workspace, and the rest is covered by each adapter's own tests. The
// process log needs a workload: RunProcesses covers it given a Workload,
// and TerminalWorkload types commands into the environment's shell. A lost
// environment needs a way to end one without a revoke: RunLoss covers it
// given a Lossy.
//
// Terminal subtests drive the shell with a small POSIX subset every adapter
// supports: NAME=value, echo with $NAME and quotes, seq, and exit.
package execenvtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// Factory returns a Host isolated enough for one conformance subtest.
// The host must advertise images "default" and "alt" and accept two grants,
// and must apply cfg.
type Factory func(t *testing.T, cfg Config) execenv.Host

// Config is what a subtest needs from the host it asks for.
type Config struct {
	// TerminalGrace is the host's terminal grace. Zero is the adapter's
	// default.
	TerminalGrace time.Duration
}

// Run exercises the portable occupancy and PTY contract.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	runControl(t, factory)
	runProjection(t, factory)
	runTerminal(t, factory)

	t.Run("ready reports the default image", func(t *testing.T) {
		host := factory(t, Config{})
		report, err := host.Ready(context.Background())
		if err != nil {
			t.Fatalf("Ready() error = %v", err)
		}
		if !report.Usable {
			t.Fatal("Ready() Usable = false")
		}
		if !hasImage(report.Images, "default") {
			t.Fatalf("Ready() Images = %v, want default", report.Images)
		}
		if !hasNetwork(report.Networks, execenv.NetworkNone) {
			t.Fatalf("Ready() Networks = %v, want NetworkNone", report.Networks)
		}
		if report.Slots < 1 {
			t.Fatalf("Ready() Slots = %d, want at least 1", report.Slots)
		}
	})

	t.Run("ensure occupies a grant", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(context.Background(), execenv.Spec{
			ID:    "grant-1",
			Image: "default",
		})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		if env.ID() != "grant-1" {
			t.Fatalf("ID() = %q, want grant-1", env.ID())
		}
	})

	t.Run("ensure reattaches the same grant", func(t *testing.T) {
		host := factory(t, Config{})
		first, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		second, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("second Ensure() error = %v", err)
		}
		if first.ID() != second.ID() {
			t.Fatalf("IDs = %q and %q", first.ID(), second.ID())
		}
	})

	t.Run("ensure rejects an unknown image", func(t *testing.T) {
		host := factory(t, Config{})
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "missing"})
		if !errors.Is(err, execenv.ErrUnknownImage) {
			t.Fatalf("Ensure() error = %v, want ErrUnknownImage", err)
		}
	})

	t.Run("existing id conflicts before catalog lookup", func(t *testing.T) {
		host := factory(t, Config{})
		if _, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
			t.Fatal(err)
		}
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "missing"})
		if !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Ensure() error = %v, want ErrConflict", err)
		}
	})

	t.Run("ensure rejects an invalid spec", func(t *testing.T) {
		host := factory(t, Config{})
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "bad id", Image: "default"})
		if !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("Ensure() error = %v, want ErrInvalid", err)
		}
	})

	t.Run("ensure conflicts when image or network change", func(t *testing.T) {
		host := factory(t, Config{})
		if _, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "alt"})
		if !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Ensure() error = %v, want ErrConflict", err)
		}
		_, err = host.Ensure(context.Background(), execenv.Spec{
			ID:      "grant-1",
			Image:   "default",
			Network: execenv.NetworkAllowlist,
		})
		if !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Ensure() network error = %v, want ErrConflict", err)
		}
	})

	t.Run("ensure refuses a third grant when only two slots exist", func(t *testing.T) {
		host := factory(t, Config{})
		if _, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		if _, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-2", Image: "default"}); err != nil {
			t.Fatalf("second Ensure() error = %v", err)
		}
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-3", Image: "default"})
		if !errors.Is(err, execenv.ErrCapacity) {
			t.Fatalf("Ensure() error = %v, want ErrCapacity", err)
		}
	})

	t.Run("attach accepts pty writes", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.AttachRequest{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		// Isolated attach is a guest shell, not an echo. Portable
		// coverage is that a write is accepted; echo is adapter-local.
		if _, err := term.Write([]byte("ping")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	})

	t.Run("second attach is busy until close", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		first, err := env.Attach(context.Background(), execenv.AttachRequest{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		_, err = env.Attach(context.Background(), execenv.AttachRequest{})
		if !errors.Is(err, execenv.ErrBusy) {
			t.Fatalf("second Attach() error = %v, want ErrBusy", err)
		}
		if err := first.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		second, err := env.Attach(context.Background(), execenv.AttachRequest{})
		if err != nil {
			t.Fatalf("Attach() after Close() error = %v", err)
		}
		_ = second.Close()
	})

	t.Run("fenced freeze blocks pty and thaw restores it", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.AttachRequest{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		request := execenv.ControlRequest{Fence: execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}, State: execenv.ControlFrozen}
		if _, err := env.Control(context.Background(), request); err != nil {
			t.Fatalf("Control(frozen) error = %v", err)
		}
		if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Write() error = %v, want ErrFrozen", err)
		}
		if _, err := env.Attach(context.Background(), execenv.AttachRequest{}); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Attach() error = %v, want ErrFrozen", err)
		}
		request.Fence.FenceRevision++
		request.State = execenv.ControlRunning
		if _, err := env.Control(context.Background(), request); err != nil {
			t.Fatalf("Control(running) error = %v", err)
		}
		if _, err := env.Attach(context.Background(), execenv.AttachRequest{}); !errors.Is(err, execenv.ErrBusy) {
			t.Fatalf("Attach after thaw = %v, want existing PTY busy", err)
		}
		if _, err := term.Write([]byte("ok")); err != nil {
			t.Fatalf("Write() after thaw error = %v", err)
		}
	})

	t.Run("revoke is idempotent and forbids later use", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.AttachRequest{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		if err := env.Revoke(context.Background()); err != nil {
			t.Fatalf("Revoke() error = %v", err)
		}
		if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("Write() after Revoke() error = %v, want ErrRevoked", err)
		}
		if err := host.Revoke(context.Background(), "grant-1"); err != nil {
			t.Fatalf("second Revoke() error = %v", err)
		}
		if _, err := env.Attach(context.Background(), execenv.AttachRequest{}); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("Attach() error = %v, want ErrRevoked", err)
		}
		_, err = host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() after revoke error = %v", err)
		}
	})

	t.Run("cancelled upload returns the context error", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
		if _, err := env.Control(context.Background(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := env.UploadProjectionContent(ctx, fence, strings.NewReader("x")); !errors.Is(err, context.Canceled) {
			t.Fatalf("UploadProjectionContent() error = %v, want context.Canceled", err)
		}
	})

	t.Run("cancelled ensure returns the context error", func(t *testing.T) {
		host := factory(t, Config{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := host.Ensure(ctx, execenv.Spec{ID: "grant-1", Image: "default"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Ensure() error = %v, want context.Canceled", err)
		}
	})
}

func hasNetwork(networks []execenv.Network, want execenv.Network) bool {
	for _, network := range networks {
		if network == want {
			return true
		}
	}
	return false
}

func hasImage(images []execenv.Image, want execenv.Image) bool {
	for _, image := range images {
		if image == want {
			return true
		}
	}
	return false
}
