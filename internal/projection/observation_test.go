package projection

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/guesttree"
)

func guestSnapshot(s *State) *guesttree.Snapshot {
	result := &guesttree.Snapshot{Nodes: make(map[string]guesttree.Node)}
	for name, node := range s.Nodes() {
		result.Nodes[name] = guesttree.Node{Identity: node.Identity, Kind: node.Kind, Data: node.Data}
	}
	return result
}
func TestConfirmedGuestWriteAdvancesBaselineWithoutErasingLaterBytes(t *testing.T) {
	s, fence := testProjection(t)
	actual := guestSnapshot(s)
	node := actual.Nodes["dir/file"]
	node.Data = []byte("later guest write")
	actual.Nodes["dir/file"] = node
	ref, err := s.Put([]byte("accepted first write"))
	if err != nil {
		t.Fatal(err)
	}
	mutation := execenv.ProjectionMutation{JournalCursor: 6, EntryID: "file-id", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "dir/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &ref}
	event := execenv.ExecutionObservation{Fence: fence, HostSequence: 1, BasedOnWorkspaceCursor: 5, Operation: execenv.ObservationWriteFile, NodeIdentity: node.Identity, Kind: execenv.ProjectionFile, Path: "dir/file", ExpectedContentVersion: "v1", Content: &ref}
	binding := execenv.ObservationProjection{Fence: fence, HostSequence: 1, Mutation: mutation}
	lookup := func(uint64) (execenv.ExecutionObservation, error) { return event, nil }
	if err := s.RegisterObservation(binding, lookup); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.GuestChanged(); err != nil {
			t.Fatal(err)
		}
	}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "confirmed", FromWorkspaceCursor: 5, ThroughWorkspaceCursor: 6, ExpectedHostCursor: 2, Mutations: []execenv.ProjectionMutation{mutation}}
	if _, err := s.Apply(request, func(plan Plan) error {
		prepared, err := plan.Prepare(actual)
		if err != nil {
			return err
		}
		if len(prepared.Batch.Mutations) != 0 || !bytes.Equal(prepared.Tree["dir/file"].Data, []byte("later guest write")) {
			t.Fatal("echo overwrote later guest bytes")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s.nodes["dir/file"].Version != "v2" || string(s.nodes["dir/file"].Data) != "accepted first write" || s.cursor != 6 {
		t.Fatal("accepted outcome did not advance baseline")
	}
	// The caller may reconnect/reupload after durable acknowledgement released
	// the event. Immutable content identity, not a disposable transfer ID, binds.
	replay := binding
	newRef := ref
	newRef.TransferID = "another-upload"
	replay.Mutation.Content = &newRef
	if err := s.RegisterObservation(replay, func(uint64) (execenv.ExecutionObservation, error) {
		t.Fatal("exact binding retry reread retired event")
		return execenv.ExecutionObservation{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	replay.Mutation.ResultingContentVersion = "changed"
	if err := s.RegisterObservation(replay, lookup); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("outcome rebound: %v", err)
	}
}
func TestConfirmedDirectoryMoveAndFollowingSavePrepareInJournalOrder(t *testing.T) {
	s, fence := testProjection(t)
	actual := guestSnapshot(s)
	dir, file := actual.Nodes["dir"], actual.Nodes["dir/file"]
	actual.Nodes = map[string]guesttree.Node{"moved": dir, "moved/file": file}
	move := execenv.ProjectionMutation{JournalCursor: 6, EntryID: "dir-id", Kind: execenv.ProjectionDirectory, Operation: execenv.ProjectionMove, Path: "dir", DestinationPath: "moved"}
	event := execenv.ExecutionObservation{Fence: fence, HostSequence: 1, Operation: execenv.ObservationMove, NodeIdentity: dir.Identity, Kind: execenv.ProjectionDirectory, Path: "dir", DestinationPath: "moved"}
	if err := s.RegisterObservation(execenv.ObservationProjection{Fence: fence, HostSequence: 1, Mutation: move}, func(uint64) (execenv.ExecutionObservation, error) { return event, nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.GuestChanged(); err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put([]byte("save after move"))
	if err != nil {
		t.Fatal(err)
	}
	write := execenv.ProjectionMutation{JournalCursor: 7, EntryID: "file-id", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "moved/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &ref}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "move-save", FromWorkspaceCursor: 5, ThroughWorkspaceCursor: 7, ExpectedHostCursor: 1, Mutations: []execenv.ProjectionMutation{move, write}}
	if _, err := s.Apply(request, func(plan Plan) error {
		prepared, err := plan.Prepare(actual)
		if err != nil {
			return err
		}
		if len(prepared.Batch.Mutations) != 1 || prepared.Batch.Mutations[0].Path != "moved/file" || string(prepared.Tree["moved/file"].Data) != "save after move" || prepared.Guest.Nodes["moved/file"].Identity != file.Identity {
			t.Fatal("mixed echo/save plan lost subtree identity or order")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestUnconfirmedOrDifferentGuestEffectCannotBecomeEcho(t *testing.T) {
	s, fence := testProjection(t)
	actual := guestSnapshot(s)
	node := actual.Nodes["dir/file"]
	node.Data = []byte("guest")
	actual.Nodes["dir/file"] = node
	ref, err := s.Put([]byte("different server bytes"))
	if err != nil {
		t.Fatal(err)
	}
	mutation := execenv.ProjectionMutation{JournalCursor: 6, EntryID: "file-id", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "dir/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &ref}
	observed, err := s.Put([]byte("guest"))
	if err != nil {
		t.Fatal(err)
	}
	event := execenv.ExecutionObservation{Fence: fence, HostSequence: 1, Operation: execenv.ObservationWriteFile, NodeIdentity: node.Identity, Kind: execenv.ProjectionFile, Path: "dir/file", Content: &observed}
	if err := s.RegisterObservation(execenv.ObservationProjection{Fence: fence, HostSequence: 1, Mutation: mutation}, func(uint64) (execenv.ExecutionObservation, error) { return event, nil }); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("different bytes registered as echo")
	}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "unconfirmed", FromWorkspaceCursor: 5, ThroughWorkspaceCursor: 6, Mutations: []execenv.ProjectionMutation{mutation}}
	if _, err := s.Apply(request, func(plan Plan) error { _, err := plan.Prepare(actual); return err }); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("unconfirmed guest effect bypassed baseline: %v", err)
	}
	if s.Cursor() != 5 {
		t.Fatal("failed conflict advanced baseline")
	}
}
