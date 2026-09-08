package memory_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func TestSemanticObservationsPreserveOriginalContentAndSubtreeIdentity(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "semantic", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Revoke(t.Context())
	semantic := env.(execenv.SemanticEnv)
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: semantic.Epoch(), FenceRevision: 1}
	if _, err := semantic.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	ref, err := semantic.UploadProjectionContent(t.Context(), fence, bytes.NewBufferString("base"))
	if err != nil {
		t.Fatal(err)
	}
	initial := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", WorkspaceCursor: 5, Entries: []execenv.ProjectionEntry{
		{EntryID: "dir", Kind: execenv.ProjectionDirectory, Path: "dir"},
		{EntryID: "file", Kind: execenv.ProjectionFile, Path: "dir/file", ContentVersion: "v1", Content: &ref},
	}}
	if _, err := semantic.InitializeProjection(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"first", "second"} {
		if err := host.WriteGuest(t.Context(), env.ID(), "dir/file", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.MoveGuest(t.Context(), env.ID(), "dir", "moved"); err != nil {
		t.Fatal(err)
	}
	if err := host.WriteGuest(t.Context(), env.ID(), "moved/file", []byte("third")); err != nil {
		t.Fatal(err)
	}
	page, err := semantic.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 4 {
		t.Fatalf("observations: %+v %v", page, err)
	}
	identity := page.Observations[0].NodeIdentity
	for i, want := range map[int]string{0: "first", 1: "second", 3: "third"} {
		event := page.Observations[i]
		if event.NodeIdentity != identity || event.ExpectedContentVersion != "v1" || event.BasedOnWorkspaceCursor != 5 {
			t.Fatalf("changed identity/baseline: %+v", event)
		}
		reader, err := semantic.OpenObservationContent(t.Context(), fence, *event.Content)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil || string(body) != want {
			t.Fatalf("captured body: %q %v", body, err)
		}
	}
	moved := page.Observations[2]
	if moved.Operation != execenv.ObservationMove || moved.Kind != execenv.ProjectionDirectory || moved.Path != "dir" || moved.DestinationPath != "moved" {
		t.Fatalf("not one subtree move: %+v", moved)
	}
	// Observation handles cannot be consumed as caller projection uploads.
	mutation := execenv.ProjectionApply{Fence: fence, MutationID: "wrong-purpose", FromWorkspaceCursor: 5, ThroughWorkspaceCursor: 6, ExpectedHostCursor: 4, Mutations: []execenv.ProjectionMutation{{JournalCursor: 6, EntryID: "other", Operation: execenv.ProjectionCreate, Kind: execenv.ProjectionFile, Path: "other", ResultingContentVersion: "v2", Content: page.Observations[0].Content}}}
	if _, err := semantic.ApplyProjection(t.Context(), mutation); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("observation transfer reused for upload: %v", err)
	}
	if err := semantic.AcknowledgeObservations(t.Context(), fence, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := semantic.OpenObservationContent(t.Context(), fence, *page.Observations[0].Content); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("acknowledged bytes still available")
	}
	if err := host.RemoveGuest(t.Context(), env.ID(), "moved/file"); err != nil {
		t.Fatal(err)
	}
	page, err = semantic.Observe(t.Context(), fence, 4, 128)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].NodeIdentity != identity || page.Observations[0].Operation != execenv.ObservationDelete {
		t.Fatalf("descendant deletion identity: %+v %v", page, err)
	}
}
