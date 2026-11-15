package memory_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func TestObservationsPreserveOriginalContentAndSubtreeIdentity(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "semantic", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer env.Revoke(t.Context())
	semantic := env
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
	// The guest's changes are captured when the caller observes, as on a
	// real guest, so observe after each one to see each one.
	captured := func() {
		t.Helper()
		if _, err := semantic.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: 0, Limit: 128}); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{"first", "second"} {
		if err := host.WriteGuest(t.Context(), env.ID(), "dir/file", []byte(body)); err != nil {
			t.Fatal(err)
		}
		captured()
	}
	if err := host.MoveGuest(t.Context(), env.ID(), "dir", "moved"); err != nil {
		t.Fatal(err)
	}
	captured()
	if err := host.WriteGuest(t.Context(), env.ID(), "moved/file", []byte("third")); err != nil {
		t.Fatal(err)
	}
	page, err := semantic.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: 0, Limit: 128})
	if err != nil || len(page.Observations) != 4 {
		t.Fatalf("observations: %+v %v", page, err)
	}
	identity := page.Observations[0].NodeIdentity
	for _, i := range []int{0, 1, 3} {
		event := page.Observations[i]
		if event.NodeIdentity != identity || event.ExpectedContentVersion != "v1" || event.BasedOnWorkspaceCursor != 5 {
			t.Fatalf("changed identity/baseline: %+v", event)
		}
	}
	// Bodies are read from the workspace: the earlier writes are stale, and
	// the newest observation reads what is there.
	for _, i := range []int{0, 1} {
		if _, err := semantic.OpenObservationContent(t.Context(), fence, *page.Observations[i].Content); !errors.Is(err, execenv.ErrStale) {
			t.Fatalf("superseded body %d = %v, want ErrStale", i, err)
		}
	}
	reader, err := semantic.OpenObservationContent(t.Context(), fence, *page.Observations[3].Content)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(body) != "third" {
		t.Fatalf("captured body: %q %v", body, err)
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
	page, err = semantic.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: 4, Limit: 128})
	if err != nil || len(page.Observations) != 1 || page.Observations[0].NodeIdentity != identity || page.Observations[0].Operation != execenv.ObservationDelete {
		t.Fatalf("descendant deletion identity: %+v %v", page, err)
	}
}

// An overwrite puts the journal's body in place of the guest's unconfirmed
// one, and the discarded observation's body is gone: reading it is stale.
// An overwriting directory create replaces an ignored directory instead of
// adopting it.
func TestOverwriteMakesTheJournalWinInPlace(t *testing.T) {
	host, env, fence := running(t)
	base, err := env.UploadProjectionContent(t.Context(), fence, bytes.NewBufferString("base"))
	if err != nil {
		t.Fatal(err)
	}
	initial := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{
		{EntryID: "file", Kind: execenv.ProjectionFile, Path: "file", ContentVersion: "v1", Content: &base},
	}, Ignore: &execenv.IgnoreRules{File: "rules", Fallback: []string{"cache/"}}}
	if _, err := env.InitializeProjection(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	if err := host.WriteGuest(t.Context(), env.ID(), "file", []byte("guest")); err != nil {
		t.Fatal(err)
	}
	if err := host.MkdirGuest(t.Context(), env.ID(), "cache"); err != nil {
		t.Fatal(err)
	}
	if err := host.WriteGuest(t.Context(), env.ID(), "cache/blob", []byte("ignored")); err != nil {
		t.Fatal(err)
	}
	page, err := env.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, Limit: 16})
	if err != nil || len(page.Observations) != 1 {
		t.Fatalf("Observe() = %+v, %v", page, err)
	}
	guest := page.Observations[0]
	body, err := env.UploadProjectionContent(t.Context(), fence, bytes.NewBufferString("journal"))
	if err != nil {
		t.Fatal(err)
	}
	apply := execenv.ProjectionApply{Fence: fence, MutationID: "win", FromWorkspaceCursor: 0, ThroughWorkspaceCursor: 2, ExpectedHostCursor: page.HostCursor, Mutations: []execenv.ProjectionMutation{
		{JournalCursor: 1, EntryID: "file", Operation: execenv.ProjectionWrite, Kind: execenv.ProjectionFile, Path: "file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &body, Overwrite: true},
		{JournalCursor: 2, EntryID: "cache", Operation: execenv.ProjectionCreate, Kind: execenv.ProjectionDirectory, Path: "cache", Overwrite: true},
	}}
	receipt, err := env.ApplyProjection(t.Context(), apply)
	if err != nil || len(receipt.Discarded) != 1 || receipt.Discarded[0] != guest.HostSequence {
		t.Fatalf("ApplyProjection(overwrite) = %+v, %v", receipt, err)
	}
	if got, err := host.ReadGuest(t.Context(), env.ID(), "file"); err != nil || string(got) != "journal" {
		t.Fatalf("guest file = %q, %v; want the journal's body", got, err)
	}
	if _, err := host.ReadGuest(t.Context(), env.ID(), "cache/blob"); !errors.Is(err, execenv.ErrNotFound) {
		t.Fatalf("an overwriting directory create adopted the ignored directory: %v", err)
	}
	reader, err := env.OpenObservationContent(t.Context(), fence, *guest.Content)
	if err == nil {
		_, err = io.ReadAll(reader)
		_ = reader.Close()
	}
	if !errors.Is(err, execenv.ErrStale) {
		t.Fatalf("reading a discarded observation's body = %v, want ErrStale", err)
	}
}
