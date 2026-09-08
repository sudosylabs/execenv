//go:build linux || darwin

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sudosylabs/execenv"
)

type testWriteBarrier struct {
	inside   bool
	poisoned bool
}

func (b *testWriteBarrier) Stopped(ctx context.Context, fn func() error) error {
	if b.poisoned {
		return execenv.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.inside = true
	defer func() { b.inside = false }()
	if err := fn(); err != nil {
		b.poisoned = true
		return err
	}
	return nil
}
func journalFixture(t *testing.T) (*diskJournal, execenv.ProjectionFence, *testWriteBarrier) {
	t.Helper()
	barrier := &testWriteBarrier{}
	j := newDiskJournal(t.TempDir(), barrier, func(string) error {
		if !barrier.inside {
			t.Fatal("disk ownership changed outside barrier")
		}
		return nil
	})
	t.Cleanup(j.close)
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	ref, err := j.upload(fence, []byte("base"))
	if err != nil {
		t.Fatal(err)
	}
	request := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", WorkspaceCursor: 10, Entries: []execenv.ProjectionEntry{{EntryID: "dir", Kind: execenv.ProjectionDirectory, Path: "dir"}, {EntryID: "file", Kind: execenv.ProjectionFile, Path: "dir/file", ContentVersion: "v1", Content: &ref}}}
	if _, err := j.initialize(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return j, fence, barrier
}
func TestDiskJournalPreservesUnrelatedGuestWriteAndExactReplay(t *testing.T) {
	j, fence, barrier := journalFixture(t)
	if err := os.WriteFile(filepath.Join(j.home, "unrelated"), []byte("guest"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := j.upload(fence, []byte("saved"))
	if err != nil {
		t.Fatal(err)
	}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "save", FromWorkspaceCursor: 10, ThroughWorkspaceCursor: 11, Mutations: []execenv.ProjectionMutation{{JournalCursor: 11, EntryID: "file", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "dir/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &ref}}}
	if _, err := j.apply(t.Context(), request); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("unseen host cursor: %v", err)
	}
	if barrier.poisoned {
		t.Fatal("safe cursor refusal destroyed resumable guest")
	}
	page, err := j.observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 1 {
		t.Fatalf("pending guest change: %+v %v", page, err)
	}
	request.ExpectedHostCursor = page.HostCursor
	receipt, err := j.apply(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(j.home, "dir/file"))
	if err != nil || string(body) != "saved" {
		t.Fatalf("saved bytes: %q %v", body, err)
	}
	if err := os.WriteFile(filepath.Join(j.home, "dir/file"), []byte("later guest"), 0600); err != nil {
		t.Fatal(err)
	}
	replay, err := j.apply(t.Context(), request)
	if err != nil || receipt != replay {
		t.Fatalf("exact retry: %+v %v", replay, err)
	}
	events, err := j.observe(t.Context(), fence, 0, 128)
	if err != nil || len(events.Observations) != 2 {
		t.Fatalf("projection echo or lost guest event: %+v %v", events, err)
	}
	if events.Observations[1].ExpectedContentVersion != "v2" || events.Observations[1].BasedOnWorkspaceCursor != 11 {
		t.Fatal("guest event lost original projected baseline")
	}
	reader, err := j.content(fence, *events.Observations[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err = io.ReadAll(reader)
	if err != nil || string(body) != "guest" {
		t.Fatalf("unrelated immutable content lost: %q %v", body, err)
	}
}
func TestDiskJournalCapturesSemanticDirectoryMove(t *testing.T) {
	j, fence, _ := journalFixture(t)
	identity := j.snapshot.Nodes["dir/file"].Identity
	if err := os.Rename(filepath.Join(j.home, "dir"), filepath.Join(j.home, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.home, "moved/file"), []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := j.observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 2 {
		t.Fatalf("directory move: %+v %v", page, err)
	}
	if page.Observations[0].Operation != execenv.ObservationMove || page.Observations[0].Kind != execenv.ProjectionDirectory || page.Observations[1].NodeIdentity != identity || page.Observations[1].ExpectedContentVersion != "v1" {
		t.Fatalf("subtree identity/version lost: %+v", page)
	}
}
func TestDiskJournalRefusesSameFileConflictAndPoisonsUncertainCommit(t *testing.T) {
	j, fence, barrier := journalFixture(t)
	if err := os.WriteFile(filepath.Join(j.home, "dir/file"), []byte("guest"), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := j.observe(t.Context(), fence, 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := j.upload(fence, []byte("save"))
	if err != nil {
		t.Fatal(err)
	}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "save", FromWorkspaceCursor: 10, ThroughWorkspaceCursor: 11, ExpectedHostCursor: page.HostCursor, Mutations: []execenv.ProjectionMutation{{JournalCursor: 11, EntryID: "file", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "dir/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &ref}}}
	if _, err := j.apply(t.Context(), request); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("guest overwritten: %v", err)
	}
	if barrier.poisoned {
		t.Fatal("precondition refusal poisoned occupancy")
	}
	// A failure after disk mutation must not claim success or allow another
	// ordinary operation to reuse uncertain state.
	request.Mutations[0] = execenv.ProjectionMutation{JournalCursor: 11, EntryID: "other", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionCreate, Path: "other", ResultingContentVersion: "v2", Content: &ref}
	j.own = func(string) error { return errors.New("ownership write failed") }
	if _, err := j.apply(t.Context(), request); err == nil {
		t.Fatal("uncertain commit acknowledged")
	}
	if !barrier.poisoned {
		t.Fatal("partially mutated disk left usable")
	}
	if j.state.Cursor() != 10 {
		t.Fatal("failed commit advanced acknowledged cursor")
	}
	if _, err := j.apply(t.Context(), request); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("poisoned retry: %v", err)
	}
}

func TestNativeConfirmedGuestCreateDoesNotOverwriteLaterWrite(t *testing.T) {
	j, fence, _ := journalFixture(t)
	name := filepath.Join(j.home, "guest")
	if err := os.WriteFile(name, []byte("accepted"), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := j.observe(t.Context(), fence, 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	event := page.Observations[0]
	if err := os.WriteFile(name, []byte("later"), 0600); err != nil {
		t.Fatal(err)
	}
	reference, err := j.upload(fence, []byte("accepted"))
	if err != nil {
		t.Fatal(err)
	}
	mutation := execenv.ProjectionMutation{JournalCursor: 11, EntryID: "guest-entry", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionCreate, Path: "guest", ResultingContentVersion: "v1", Content: &reference}
	binding := execenv.ObservationProjection{Fence: fence, HostSequence: event.HostSequence, Mutation: mutation}
	if err := j.confirm(binding); err != nil {
		t.Fatal(err)
	}
	if err := j.acknowledge(fence, event.HostSequence); err != nil {
		t.Fatal(err)
	}
	request := execenv.ProjectionApply{Fence: fence, MutationID: "accepted-guest", FromWorkspaceCursor: 10, ThroughWorkspaceCursor: 11, ExpectedHostCursor: 1, Mutations: []execenv.ProjectionMutation{mutation}}
	if _, err := j.apply(t.Context(), request); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("later unseen guest cursor: %v", err)
	}
	request.ExpectedHostCursor = 2
	if _, err := j.apply(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(name)
	if err != nil || string(body) != "later" {
		t.Fatalf("accepted create replay overwrote later write: %q %v", body, err)
	}
	if j.state.Nodes()["guest"].Identity != event.NodeIdentity {
		t.Fatal("guest identity not adopted into projection")
	}
	page, err = j.observe(t.Context(), fence, 1, 128)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Operation != execenv.ObservationWriteFile {
		t.Fatalf("later original event lost: %+v %v", page, err)
	}
	if err := j.confirm(binding); err != nil {
		t.Fatalf("binding retry after acknowledgement: %v", err)
	}
}
