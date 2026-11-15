//go:build linux || darwin

package journal

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// journalWorkspace drives a disk journal through the conformance surface.
type journalWorkspace struct{ j *Journal }

func (w journalWorkspace) UploadProjectionContent(_ context.Context, fence execenv.ProjectionFence, body io.Reader) (execenv.ContentReference, error) {
	blob, err := w.j.Receive(body)
	if err != nil {
		return execenv.ContentReference{}, err
	}
	return w.j.Upload(fence, blob)
}
func (w journalWorkspace) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	return w.j.Initialize(ctx, request)
}
func (w journalWorkspace) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	return w.j.Apply(ctx, request)
}
func (w journalWorkspace) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	return w.j.Observe(ctx, fence, after, limit)
}

// diskGuest writes Home directly, as guest processes do.
type diskGuest struct{ home string }

func (g diskGuest) full(name string) string { return filepath.Join(g.home, filepath.FromSlash(name)) }
func (g diskGuest) Write(_ context.Context, name string, data []byte) error {
	return os.WriteFile(g.full(name), data, 0o644)
}
func (g diskGuest) Mkdir(_ context.Context, name string) error { return os.Mkdir(g.full(name), 0o755) }
func (g diskGuest) Move(_ context.Context, from, to string) error {
	return os.Rename(g.full(from), g.full(to))
}
func (g diskGuest) Remove(_ context.Context, name string) error { return os.RemoveAll(g.full(name)) }

func ignoreJournal(t *testing.T, own func(string) error) (*Journal, execenv.ProjectionFence) {
	t.Helper()
	j := New(t.TempDir(), testStore(t), &testWriteBarrier{}, own)
	t.Cleanup(j.Close)
	return j, execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
}

func TestDiskJournalIgnoreConformance(t *testing.T) {
	execenvtest.RunIgnore(t, func(t *testing.T) execenvtest.Observed {
		j, fence := ignoreJournal(t, nil)
		return execenvtest.Observed{Workspace: journalWorkspace{j}, Fence: fence, Guest: diskGuest{j.home}}
	})
}

// An ignored directory is never opened: unreadable contents, links and
// hard-linked files under it cannot lag the capture, and every byte stays on
// disk. Ownership walks only the projected subtrees.
func TestDiskJournalNeverWalksAnIgnoredDirectory(t *testing.T) {
	var owned []string
	j, fence := ignoreJournal(t, func(name string) error { owned = append(owned, name); return nil })
	init := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Ignore: &execenv.IgnoreRules{File: "rules", Fallback: []string{"deps/", "*.lnk"}}}
	if _, err := j.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	guest := diskGuest{j.home}
	deps := guest.full("deps")
	if err := os.MkdirAll(filepath.Join(deps, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deps, "pkg", "index"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(deps, "pkg", "index"), filepath.Join(deps, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pkg", filepath.Join(deps, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("deps", guest.full("top.lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(deps, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(deps, 0o755) })
	page, err := j.Observe(t.Context(), fence, 0, 16)
	if err != nil || len(page.Observations) != 0 {
		t.Fatalf("ignored tree observed: %+v %v", page, err)
	}
	ref, err := testUpload(t, j, fence, "projected")
	if err != nil {
		t.Fatal(err)
	}
	owned = nil
	apply := execenv.ProjectionApply{Fence: fence, MutationID: "create", FromWorkspaceCursor: 0, ThroughWorkspaceCursor: 1, Mutations: []execenv.ProjectionMutation{{JournalCursor: 1, EntryID: "file", Operation: execenv.ProjectionCreate, Kind: execenv.ProjectionFile, Path: "file", ResultingContentVersion: "v1", Content: &ref}}}
	if _, err := j.Apply(t.Context(), apply); err != nil {
		t.Fatal(err)
	}
	for _, name := range owned {
		if strings.HasPrefix(name, deps) {
			t.Fatalf("ownership walked an ignored directory: %s", name)
		}
	}
	if err := os.Chmod(deps, 0o755); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(deps, "pkg", "index")); err != nil || string(body) != "x" {
		t.Fatalf("ignored file lost: %q %v", body, err)
	}
}

// A projected directory adopts an ignored guest directory at its path, and a
// held file moved under an ignored name stays observed.
func TestDiskJournalProjectionAdoptsIgnoredDirectory(t *testing.T) {
	j, fence := ignoreJournal(t, nil)
	init := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Ignore: &execenv.IgnoreRules{File: "rules", Fallback: []string{"cache/"}}}
	if _, err := j.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	guest := diskGuest{j.home}
	if err := guest.Mkdir(t.Context(), "cache"); err != nil {
		t.Fatal(err)
	}
	if err := guest.Write(t.Context(), "cache/blob", []byte("guest")); err != nil {
		t.Fatal(err)
	}
	ref, err := testUpload(t, j, fence, "projected")
	if err != nil {
		t.Fatal(err)
	}
	apply := execenv.ProjectionApply{Fence: fence, MutationID: "adopt", FromWorkspaceCursor: 0, ThroughWorkspaceCursor: 2, Mutations: []execenv.ProjectionMutation{
		{JournalCursor: 1, EntryID: "cache", Operation: execenv.ProjectionCreate, Kind: execenv.ProjectionDirectory, Path: "cache"},
		{JournalCursor: 2, EntryID: "held", Operation: execenv.ProjectionCreate, Kind: execenv.ProjectionFile, Path: "cache/held", ResultingContentVersion: "v1", Content: &ref},
	}}
	if _, err := j.Apply(t.Context(), apply); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(guest.full("cache/blob")); err != nil || string(body) != "guest" {
		t.Fatalf("adopted directory lost its ignored file: %q %v", body, err)
	}
	if err := guest.Write(t.Context(), "cache/held", []byte("edited")); err != nil {
		t.Fatal(err)
	}
	if err := guest.Write(t.Context(), "cache/blob", []byte("still ignored")); err != nil {
		t.Fatal(err)
	}
	page, err := j.Observe(t.Context(), fence, 0, 16)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Operation != execenv.ObservationWriteFile || page.Observations[0].Path != "cache/held" {
		t.Fatalf("observations = %+v %v", page, err)
	}
}
