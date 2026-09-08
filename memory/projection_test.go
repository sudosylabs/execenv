package memory_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func TestJournalDoesNotOverwriteGuestChanges(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "journal-conflict", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Revoke(t.Context())
	projection := env.(execenv.ProjectionEnv)
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: projection.Epoch(), FenceRevision: 1}
	if _, err := projection.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	ref, err := projection.UploadProjectionContent(t.Context(), fence, bytes.NewBufferString("base"))
	if err != nil {
		t.Fatal(err)
	}
	initial := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{
		{EntryID: "directory", Kind: execenv.ProjectionDirectory, Path: "dir"},
		{EntryID: "file", Kind: execenv.ProjectionFile, Path: "dir/file", ContentVersion: "v1", Content: &ref},
	}}
	if _, err := projection.InitializeProjection(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	// Invalid guest operations do not advance the host cursor.
	if err := host.WriteGuest(t.Context(), env.ID(), "dir", []byte("bad")); err == nil {
		t.Fatal("replaced directory with a file")
	}
	if err := host.MoveGuest(t.Context(), env.ID(), "dir", "dir/child"); err == nil {
		t.Fatal("moved directory into itself")
	}
	if err := host.WriteGuest(t.Context(), env.ID(), "dir/file", []byte("guest")); err != nil {
		t.Fatal(err)
	}
	ref, err = projection.UploadProjectionContent(t.Context(), fence, bytes.NewBufferString("server"))
	if err != nil {
		t.Fatal(err)
	}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "save", ThroughWorkspaceCursor: 1, Mutations: []execenv.ProjectionMutation{
		{JournalCursor: 1, EntryID: "file", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "dir/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &ref},
	}}
	if _, err := projection.ApplyProjection(t.Context(), request); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("unseen guest cursor: %v", err)
	}
	request.ExpectedHostCursor = 1
	if _, err := projection.ApplyProjection(t.Context(), request); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("affected guest overwrite: %v", err)
	}
	// A known unrelated guest change need not prevent independent projection.
	request.Mutations[0] = execenv.ProjectionMutation{JournalCursor: 1, EntryID: "another", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionCreate, Path: "another", ResultingContentVersion: "v2", Content: &ref}
	receipt, err := projection.ApplyProjection(t.Context(), request)
	if err != nil || receipt.HostCursor != 1 {
		t.Fatalf("unrelated projection: %+v %v", receipt, err)
	}
	reader, err := env.Open(t.Context(), "dir/file")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != "guest" {
		t.Fatalf("lost unrelated guest change: %q %v", got, err)
	}
}
