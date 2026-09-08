package projection

import (
	"errors"
	"github.com/sudosylabs/execenv"
	"testing"
)

func testProjection(t *testing.T) (*State, execenv.ProjectionFence) {
	t.Helper()
	s := New()
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	ref, err := s.Put([]byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Initialize(execenv.ProjectionInitialization{Fence: fence, MutationID: "init", WorkspaceCursor: 5, Entries: []execenv.ProjectionEntry{{EntryID: "dir-id", Kind: execenv.ProjectionDirectory, Path: "dir"}, {EntryID: "file-id", Kind: execenv.ProjectionFile, Path: "dir/file", ContentVersion: "v1", Content: &ref}}}, func(execenv.Tree) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s, fence
}
func TestApplyValidatesWholeBatchBeforeCommitAndPreservesReceipts(t *testing.T) {
	s, fence := testProjection(t)
	ref, err := s.Put([]byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "save", FromWorkspaceCursor: 5, ThroughWorkspaceCursor: 7, Mutations: []execenv.ProjectionMutation{
		{JournalCursor: 6, EntryID: "dir-id", Kind: execenv.ProjectionDirectory, Operation: execenv.ProjectionMove, Path: "dir", DestinationPath: "moved"},
		{JournalCursor: 7, EntryID: "file-id", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "moved/file", ExpectedContentVersion: "wrong", ResultingContentVersion: "v2", Content: &ref},
	}}
	commits := 0
	commit := func(Plan) error { commits++; return nil }
	if _, err := s.Apply(request, commit); !errors.Is(err, execenv.ErrConflict) || commits != 0 {
		t.Fatalf("partial invalid commit: %v %d", err, commits)
	}
	if s.Cursor() != 5 || s.nodes["dir/file"].Version != "v1" {
		t.Fatal("failed batch changed state")
	}
	request.Mutations[1].ExpectedContentVersion = "v1"
	failed := errors.New("disk commit failed")
	if _, err := s.Apply(request, func(Plan) error { return failed }); !errors.Is(err, failed) || s.Cursor() != 5 {
		t.Fatalf("failed disk acknowledged: %v", err)
	}
	receipt, err := s.Apply(request, commit)
	if err != nil || receipt.AppliedWorkspaceCursor != 7 || commits != 1 {
		t.Fatalf("commit: %+v %v", receipt, err)
	}
	replay, err := s.Apply(request, commit)
	if err != nil || replay != receipt || commits != 1 {
		t.Fatalf("retry repeated effect: %+v %v", replay, err)
	}
	request.Mutations[1].ResultingContentVersion = "different"
	if _, err := s.Apply(request, commit); !errors.Is(err, execenv.ErrConflict) || commits != 1 {
		t.Fatalf("changed retry accepted: %v", err)
	}
	if s.nodes["moved/file"].EntryID != "file-id" || s.nodes["moved/file"].Version != "v2" {
		t.Fatal("subtree identity/version lost")
	}
}
func TestContentReferencesAreImmutableScopedAndConsumedOnlyOnSuccess(t *testing.T) {
	s := New()
	body := []byte("bytes")
	ref, err := s.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	body[0] = 'X'
	got, err := s.content(&ref)
	if err != nil || string(got) != "bytes" {
		t.Fatal("upload retains mutable caller bytes")
	}
	other := New()
	if _, err := other.content(&ref); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("cross-environment content accepted")
	}
	changed := ref
	changed.Size++
	if _, err := s.content(&changed); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("descriptor rebinding accepted")
	}
	s.consume([]*execenv.ContentReference{&ref})
	if _, err := s.content(&ref); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("consumed upload reusable")
	}
}
func TestHostCursorAndEntryIdentityAreIndependentPreconditions(t *testing.T) {
	s, fence := testProjection(t)
	if err := s.GuestChanged(); err != nil {
		t.Fatal(err)
	}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "delete", FromWorkspaceCursor: 5, ThroughWorkspaceCursor: 6, Mutations: []execenv.ProjectionMutation{{JournalCursor: 6, EntryID: "file-id", Operation: execenv.ProjectionDelete, Kind: execenv.ProjectionFile, Path: "dir/file", ExpectedContentVersion: "v1"}}}
	commit := func(Plan) error { t.Fatal("conflicting effect executed"); return nil }
	if _, err := s.Apply(request, commit); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("stale host cursor accepted")
	}
	request.ExpectedHostCursor = 1
	request.Mutations[0].EntryID = "replacement-id"
	if _, err := s.Apply(request, commit); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("wrong entry identity accepted")
	}
}
func TestInitializationRequiresOrderedParentsAndUniqueIdentities(t *testing.T) {
	s := New()
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	request := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{{EntryID: "child", Path: "parent/child", Kind: execenv.ProjectionDirectory}, {EntryID: "parent", Path: "parent", Kind: execenv.ProjectionDirectory}}}
	if _, err := s.Initialize(request, func(execenv.Tree) error { t.Fatal("unordered parent committed"); return nil }); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal(err)
	}
	request.Entries[0], request.Entries[1] = request.Entries[1], request.Entries[0]
	request.Entries[1].EntryID = "parent"
	if _, err := s.Initialize(request, func(execenv.Tree) error { t.Fatal("duplicate identity committed"); return nil }); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal(err)
	}
}
