package execenvtest

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

func runProjection(t *testing.T, factory Factory) {
	t.Run("journal projection preserves terminal and exact retry", func(t *testing.T) {
		host := factory(t, Config{})
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "journal", Image: "default"})
		if err != nil {
			t.Fatal(err)
		}
		projected := env
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: projected.Epoch(), FenceRevision: 1}
		if _, err := projected.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		body := bytes.Repeat([]byte("initial\n"), 20000)
		content, err := projected.UploadProjectionContent(t.Context(), fence, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		initial := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", WorkspaceCursor: 10, Entries: []execenv.ProjectionEntry{{EntryID: "dir-id", Path: "dir", Kind: execenv.ProjectionDirectory}, {EntryID: "file-id", Path: "dir/file", Kind: execenv.ProjectionFile, ContentVersion: "v1", Content: &content}}}
		receipt, err := projected.InitializeProjection(t.Context(), initial)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := projected.InitializeProjection(t.Context(), initial)
		if err != nil || receipt != replay {
			t.Fatalf("initialization retry: %+v %v", replay, err)
		}
		sh := StartShell(t, env, fence, "sh")
		saved := bytes.Repeat([]byte("saved\n"), 20000)
		content, err = projected.UploadProjectionContent(t.Context(), fence, bytes.NewReader(saved))
		if err != nil {
			t.Fatal(err)
		}
		apply := execenv.ProjectionApply{Fence: fence, MutationID: "save", FromWorkspaceCursor: 10, ThroughWorkspaceCursor: 12, Mutations: []execenv.ProjectionMutation{
			{JournalCursor: 11, EntryID: "dir-id", Operation: execenv.ProjectionMove, Kind: execenv.ProjectionDirectory, Path: "dir", DestinationPath: "moved"},
			{JournalCursor: 12, EntryID: "file-id", Operation: execenv.ProjectionWrite, Kind: execenv.ProjectionFile, Path: "moved/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &content},
		}}
		receipt, err = projected.ApplyProjection(t.Context(), apply)
		if err != nil || receipt.AppliedWorkspaceCursor != 12 {
			t.Fatalf("apply: %+v %v", receipt, err)
		}
		replay, err = projected.ApplyProjection(t.Context(), apply)
		if err != nil || receipt != replay {
			t.Fatalf("apply retry: %+v %v", replay, err)
		}
		if err := sh.Type("echo same shell"); err != nil {
			t.Fatalf("shell lost on save: %v", err)
		}
		if info, err := env.Process(t.Context(), "sh"); err != nil || !info.Running {
			t.Fatalf("save ended the shell: %+v %v", info, err)
		}
		apply.Mutations[1].ResultingContentVersion = "v3"
		if _, err := projected.ApplyProjection(t.Context(), apply); !errors.Is(err, execenv.ErrConflict) {
			t.Fatal("mutation ID rebound")
		}
		freeze := fence
		freeze.FenceRevision++
		if _, err := projected.Control(t.Context(), execenv.ControlRequest{Fence: freeze, State: execenv.ControlFrozen}); err != nil {
			t.Fatal(err)
		}
		if _, err := projected.ApplyProjection(t.Context(), apply); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("old revision accepted: %v", err)
		}
		if err := env.Revoke(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
