package execenvtest

import (
	"errors"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// tick is the timeout the lifetime subtests use; they wait a few of them.
const tick = 150 * time.Millisecond

func runResources(t *testing.T, factory Factory) {
	t.Run("a grant reserves its resources until it is revoked", func(t *testing.T) {
		host := factory(t, Config{})
		before, err := host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "sized", Image: "default"})
		if err != nil {
			t.Fatal(err)
		}
		held, err := host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		want := before.Defaults
		if held.CPUMillis.Reserved != want.CPUMillis || held.MemoryBytes.Reserved != want.MemoryBytes || held.DiskBytes.Reserved != want.DiskBytes {
			t.Fatalf("reserved = %+v %+v %+v, want the defaults %+v", held.CPUMillis, held.MemoryBytes, held.DiskBytes, want)
		}
		if held.MemoryBytes.Free != before.MemoryBytes.Free-want.MemoryBytes {
			t.Fatalf("free memory = %d, want %d", held.MemoryBytes.Free, before.MemoryBytes.Free-want.MemoryBytes)
		}
		if err := env.Revoke(t.Context()); err != nil {
			t.Fatal(err)
		}
		after, err := host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.MemoryBytes != before.MemoryBytes || after.CPUMillis != before.CPUMillis || after.DiskBytes != before.DiskBytes {
			t.Fatal("revoke did not return the grant's resources")
		}
	})

	t.Run("a grant larger than what is free is refused with the short resource", func(t *testing.T) {
		host := factory(t, Config{})
		report, err := host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		big := report.Defaults
		big.MemoryBytes = report.MemoryBytes.Free + 1<<20
		_, err = host.Ensure(t.Context(), execenv.Spec{ID: "too-big", Image: "default", Resources: big})
		var short *execenv.CapacityError
		if !errors.As(err, &short) || !errors.Is(err, execenv.ErrCapacity) || short.Resource != execenv.ResourceMemory || short.Requested != big.MemoryBytes {
			t.Fatalf("Ensure() error = %v, want memory short", err)
		}
		if after, _ := host.Ready(t.Context()); after.MemoryBytes.Reserved != 0 {
			t.Fatal("a refused grant kept a reservation")
		}
	})

	t.Run("ensure matches defaults spelled out and conflicts on another size", func(t *testing.T) {
		host := factory(t, Config{})
		report, err := host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "sized", Image: "default"}); err != nil {
			t.Fatal(err)
		}
		if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "sized", Image: "default", Resources: report.Defaults}); err != nil {
			t.Fatalf("Ensure() with the defaults spelled out = %v", err)
		}
		other := report.Defaults
		other.CPUMillis = execenv.MinGrantCPUMillis
		if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "sized", Image: "default", Resources: other}); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Ensure() with another size = %v, want ErrConflict", err)
		}
		limited := execenv.Spec{ID: "sized", Image: "default", Limits: execenv.Limits{DiskBytes: execenv.TokenBucket{Rate: 1 << 20}}}
		if _, err := host.Ensure(t.Context(), limited); err != nil {
			t.Fatalf("Ensure() with other limits = %v, want the same grant", err)
		}
	})

	t.Run("ensure refuses sizes below the minimums", func(t *testing.T) {
		host := factory(t, Config{})
		_, err := host.Ensure(t.Context(), execenv.Spec{ID: "tiny", Image: "default", Resources: execenv.Resources{MemoryBytes: 1 << 20}})
		if !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("Ensure() error = %v, want ErrInvalid", err)
		}
	})

	t.Run("limits change under the current fence", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "limited")
		limits := execenv.Limits{DiskOps: execenv.TokenBucket{Rate: 1000, Burst: 2000}, DiskBytes: execenv.TokenBucket{Rate: 8 << 20}}
		if err := env.UpdateLimits(t.Context(), fence, limits); err != nil {
			t.Fatalf("UpdateLimits() error = %v", err)
		}
		stale := fence
		stale.FenceRevision++
		if err := env.UpdateLimits(t.Context(), stale, limits); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("UpdateLimits() under a stale fence = %v, want ErrConflict", err)
		}
		bad := execenv.Limits{DiskBytes: execenv.TokenBucket{Rate: -1}}
		if err := env.UpdateLimits(t.Context(), fence, bad); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("UpdateLimits() negative = %v, want ErrInvalid", err)
		}
	})

	t.Run("the wall timeout revokes the grant", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "walled", Image: "default", WallTimeout: tick})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "revoke at the wall timeout", func() bool {
			_, err := env.ControlStatus(t.Context())
			report, _ := host.Ready(t.Context())
			return report.Grants == 0 && (err == nil || errors.Is(err, execenv.ErrRevoked))
		})
		if _, err := env.ListProcesses(t.Context()); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("ListProcesses() after the wall timeout = %v, want ErrRevoked", err)
		}
	})

	t.Run("an idle grant is revoked, but not while a process is attached", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "idle", Image: "default", IdleTimeout: tick, IdleAction: execenv.IdleRevoke})
		if err != nil {
			t.Fatal(err)
		}
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.Start(t.Context(), fence, execenv.ProcessSpec{Tag: "sh", PTY: &execenv.Window{}}); err != nil {
			t.Fatal(err)
		}
		term, err := env.AttachProcess(t.Context(), "sh", nil)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * tick)
		if report, _ := host.Ready(t.Context()); report.Grants != 1 {
			t.Fatal("a grant with an attached process was revoked as idle")
		}
		_ = term.Close()
		waitFor(t, "revoke after the idle timeout", func() bool {
			report, _ := host.Ready(t.Context())
			return report.Grants == 0
		})
	})

	t.Run("reading observations does not keep a grant awake", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "watched", Image: "default", IdleTimeout: tick, IdleAction: execenv.IdleRevoke})
		if err != nil {
			t.Fatal(err)
		}
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init"}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "revoke while observations are read", func() bool {
			_, _ = env.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, Limit: 16, Wait: tick / 5})
			_, _ = env.ObserveProcesses(t.Context(), fence, 0, 1)
			report, _ := host.Ready(t.Context())
			return report.Grants == 0
		})
	})

	t.Run("an idle grant freezes and its caller thaws it with the next revision", func(t *testing.T) {

		host := factory(t, Config{})
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "dozing", Image: "default", IdleTimeout: tick, IdleAction: execenv.IdleFreeze})
		if err != nil {
			t.Fatal(err)
		}
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "freeze after the idle timeout", func() bool {
			receipt, err := env.ControlStatus(t.Context())
			return err == nil && receipt.State == execenv.ControlFrozen && receipt.Confirmed
		})
		fence.FenceRevision++
		receipt, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning})
		if err != nil || receipt.State != execenv.ControlRunning || !receipt.Confirmed {
			t.Fatalf("thaw after an idle freeze = %+v, %v", receipt, err)
		}
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * tick * 4)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(tick / 5)
	}
	t.Fatalf("timed out waiting for %s", what)
}
