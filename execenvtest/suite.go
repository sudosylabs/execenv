// Package execenvtest contains a reusable Host occupancy and PTY conformance suite.
package execenvtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
)

// Factory returns a Host isolated enough for one conformance subtest.
// The host must advertise images "default" and "alt" and accept two grants.
type Factory func(t *testing.T) execenv.Host

// Run exercises the portable occupancy and PTY contract.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("ready reports the default image", func(t *testing.T) {
		host := factory(t)
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
		if report.Slots < 1 {
			t.Fatalf("Ready() Slots = %d, want at least 1", report.Slots)
		}
	})

	t.Run("ensure occupies a grant", func(t *testing.T) {
		host := factory(t)
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
		host := factory(t)
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
		host := factory(t)
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "missing"})
		if !errors.Is(err, execenv.ErrUnknownImage) {
			t.Fatalf("Ensure() error = %v, want ErrUnknownImage", err)
		}
	})

	t.Run("ensure rejects an invalid spec", func(t *testing.T) {
		host := factory(t)
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "bad id", Image: "default"})
		if !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("Ensure() error = %v, want ErrInvalid", err)
		}
	})

	t.Run("ensure conflicts when image or network change", func(t *testing.T) {
		host := factory(t)
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
		host := factory(t)
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

	t.Run("attach echoes pty bytes", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		if _, err := term.Write([]byte("ping")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		got := make([]byte, 4)
		if _, err := io.ReadFull(term, got); err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if !bytes.Equal(got, []byte("ping")) {
			t.Fatal("Read() returned unexpected bytes")
		}
	})

	t.Run("second attach is busy until close", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		first, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		_, err = env.Attach(context.Background(), execenv.Window{})
		if !errors.Is(err, execenv.ErrBusy) {
			t.Fatalf("second Attach() error = %v, want ErrBusy", err)
		}
		if err := first.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		second, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() after Close() error = %v", err)
		}
		_ = second.Close()
	})

	t.Run("freeze blocks pty and thaw restores it", func(t *testing.T) {
		host := factory(t)
		if !host.Capabilities().Freeze {
			t.Skip("host does not advertise freeze")
		}
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		if err := env.Freeze(context.Background()); err != nil {
			t.Fatalf("Freeze() error = %v", err)
		}
		if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Write() error = %v, want ErrFrozen", err)
		}
		if _, err := env.Attach(context.Background(), execenv.Window{}); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Attach() error = %v, want ErrFrozen", err)
		}
		if err := env.Thaw(context.Background()); err != nil {
			t.Fatalf("Thaw() error = %v", err)
		}
		if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Write() after Thaw() on old terminal error = %v, want ErrFrozen", err)
		}
		term, err = env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() after Thaw() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		if _, err := term.Write([]byte("ok")); err != nil {
			t.Fatalf("Write() after Thaw() error = %v", err)
		}
	})

	t.Run("revoke is idempotent and forbids later use", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.Window{})
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
		if _, err := env.Attach(context.Background(), execenv.Window{}); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("Attach() error = %v, want ErrRevoked", err)
		}
		_, err = host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() after revoke error = %v", err)
		}
	})

	t.Run("cancelled ensure returns the context error", func(t *testing.T) {
		host := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := host.Ensure(ctx, execenv.Spec{ID: "grant-1", Image: "default"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Ensure() error = %v, want context.Canceled", err)
		}
	})
}

func hasImage(images []execenv.Image, want execenv.Image) bool {
	for _, image := range images {
		if image == want {
			return true
		}
	}
	return false
}
