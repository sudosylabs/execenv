package remote_test

import (
	"context"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

// innerGuest acts as one grant's guest on the served memory host.
type innerGuest struct {
	host *memory.Host
	id   execenv.ID
}

func (g innerGuest) Write(ctx context.Context, path string, data []byte) error {
	return g.host.WriteGuest(ctx, g.id, path, data)
}
func (g innerGuest) Mkdir(ctx context.Context, path string) error {
	return g.host.MkdirGuest(ctx, g.id, path)
}
func (g innerGuest) Move(ctx context.Context, from, to string) error {
	return g.host.MoveGuest(ctx, g.id, from, to)
}
func (g innerGuest) Remove(ctx context.Context, path string) error {
	return g.host.RemoveGuest(ctx, g.id, path)
}

// The remote wire carries IgnoreRules through InitializeProjection.
func TestIgnoreConformance(t *testing.T) {
	execenvtest.RunIgnore(t, func(t *testing.T) execenvtest.Observed {
		t.Helper()
		inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(1)})
		if err != nil {
			t.Fatal(err)
		}
		env, err := serveInner(t, inner).Ensure(t.Context(), execenv.Spec{ID: "ignore", Image: "default"})
		if err != nil {
			t.Fatal(err)
		}
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		return execenvtest.Observed{Workspace: env, Fence: fence, Guest: innerGuest{host: inner, id: env.ID()}}
	})
}
