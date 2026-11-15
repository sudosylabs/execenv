// Package execenvtest contains a reusable Host conformance suite.
//
// Run covers occupancy, processes and their screens, fenced control,
// resources, the journal projection, seeds, files and the grant watches. Guest-originated observations need a real or simulated guest:
// RunIgnore covers IgnoreRules given a Guest that acts inside the
// workspace, and the rest is covered by each adapter's own tests. The
// process log needs a workload: RunProcesses covers it given a Workload,
// and ShellWorkload types commands into a shell in the environment. A lost
// environment needs a way to end one without a revoke: RunLoss covers it
// given a Lossy.
//
// Process subtests drive shells and commands with a small POSIX subset every
// adapter supports: NAME=value, echo with $NAME and quotes, seq, true,
// false, and exit.
package execenvtest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

// Factory returns a Host isolated enough for one conformance subtest.
// The host must advertise images "default" and "alt", must apply cfg, and
// must have room for exactly two grants of its default size: twice its
// Defaults in every resource, and grant maximums no smaller than Defaults.
type Factory func(t *testing.T, cfg Config) execenv.Host

// Config is what a subtest needs from the host it asks for.
type Config struct {
	// Nothing yet: the host is any host the factory builds.
}

// Run exercises the portable occupancy and PTY contract.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	runControl(t, factory)
	runProjection(t, factory)
	runProcessAPI(t, factory)
	runResources(t, factory)
	runContent(t, factory)
	runSeeds(t, factory)
	runFiles(t, factory)
	runGrants(t, factory)

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
		for _, c := range []execenv.Capacity{report.CPUMillis, report.MemoryBytes, report.DiskBytes} {
			if c.Total <= 0 || c.Free != c.Total || c.Reserved != 0 {
				t.Fatalf("Ready() capacity = %+v, want all of it free", c)
			}
		}
		if report.Defaults.CPUMillis <= 0 || report.Defaults.MemoryBytes <= 0 || report.Defaults.DiskBytes <= 0 {
			t.Fatalf("Ready() Defaults = %+v", report.Defaults)
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

	t.Run("ensure refuses a third grant when only two fit", func(t *testing.T) {
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
		var short *execenv.CapacityError
		if !errors.As(err, &short) || short.Free != 0 || short.Requested <= 0 {
			t.Fatalf("Ensure() error = %#v, want the short resource with nothing free", err)
		}
	})

	t.Run("revoke is idempotent and forbids later use", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "grant-1")
		sh := StartShell(t, env, fence, "sh")
		if err := env.Revoke(context.Background()); err != nil {
			t.Fatalf("Revoke() error = %v", err)
		}
		if err := sh.Type("x"); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("WriteInput() after Revoke() error = %v, want ErrRevoked", err)
		}
		if err := host.Revoke(context.Background(), "grant-1"); err != nil {
			t.Fatalf("second Revoke() error = %v", err)
		}
		if _, err := env.AttachProcess(context.Background(), "sh", nil); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("AttachProcess() error = %v, want ErrRevoked", err)
		}
		if _, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
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
