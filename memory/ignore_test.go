package memory_test

import (
	"context"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

// memoryGuest acts as one grant's guest through the adapter's test hooks.
type memoryGuest struct {
	host *memory.Host
	id   execenv.ID
}

func (g memoryGuest) Write(ctx context.Context, path string, data []byte) error {
	return g.host.WriteGuest(ctx, g.id, path, data)
}
func (g memoryGuest) Mkdir(ctx context.Context, path string) error {
	return g.host.MkdirGuest(ctx, g.id, path)
}
func (g memoryGuest) Move(ctx context.Context, from, to string) error {
	return g.host.MoveGuest(ctx, g.id, from, to)
}
func (g memoryGuest) Remove(ctx context.Context, path string) error {
	return g.host.RemoveGuest(ctx, g.id, path)
}

func running(t *testing.T) (*memory.Host, execenv.Env, execenv.ProjectionFence) {
	t.Helper()
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(1)})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "ignore", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	return host, env, fence
}

func TestIgnoreConformance(t *testing.T) {
	execenvtest.RunIgnore(t, func(t *testing.T) execenvtest.Observed {
		host, env, fence := running(t)
		return execenvtest.Observed{Workspace: env, Fence: fence, Guest: memoryGuest{host: host, id: env.ID()}}
	})
}

func TestIgnoredPathsStayInTheGuest(t *testing.T) {
	host, env, fence := running(t)
	request := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{{EntryID: "dir", Kind: execenv.ProjectionDirectory, Path: "dir"}}, Ignore: &execenv.IgnoreRules{File: "rules", Fallback: []string{"*.o"}}}
	if _, err := env.InitializeProjection(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := host.WriteGuest(t.Context(), env.ID(), "dir/a.o", []byte("object")); err != nil {
		t.Fatal(err)
	}
	// A projected move of the held directory carries the ignored file along.
	apply := execenv.ProjectionApply{Fence: fence, MutationID: "move", FromWorkspaceCursor: 0, ThroughWorkspaceCursor: 1, Mutations: []execenv.ProjectionMutation{{JournalCursor: 1, EntryID: "dir", Operation: execenv.ProjectionMove, Kind: execenv.ProjectionDirectory, Path: "dir", DestinationPath: "moved"}}}
	if _, err := env.ApplyProjection(t.Context(), apply); err != nil {
		t.Fatal(err)
	}
	body, err := host.ReadGuest(t.Context(), env.ID(), "moved/a.o")
	if err != nil || string(body) != "object" {
		t.Fatalf("ignored file lost: %q %v", body, err)
	}
	page, err := env.Observe(t.Context(), fence, 0, 16)
	if err != nil || len(page.Observations) != 0 {
		t.Fatalf("ignored file observed: %+v %v", page, err)
	}
}
