//go:build linux || darwin

package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sudosylabs/execenv"
)

func seededRef(body string) *execenv.ContentReference {
	sum := sha256.Sum256([]byte(body))
	return &execenv.ContentReference{Size: int64(len(body)), Digest: hex.EncodeToString(sum[:])}
}

// Initializing a seeded workspace writes only what differs from the seed.
func TestInitializationReconcilesASeededWorkspace(t *testing.T) {
	home := t.TempDir()
	for name, body := range map[string]string{"src/main.py": "print('hi')", "stale.txt": "stale", "src/old.py": "old"} {
		full := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seededInfo, err := os.Stat(filepath.Join(home, "src/main.py"))
	if err != nil {
		t.Fatal(err)
	}
	j := New(home, testStore(t), &testWriteBarrier{}, nil)
	t.Cleanup(j.Close)
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	added, err := testUpload(t, j, fence, "added")
	if err != nil {
		t.Fatal(err)
	}
	missing := execenv.ProjectionInitialization{Fence: fence, MutationID: "missing", Entries: []execenv.ProjectionEntry{
		{EntryID: "src", Kind: execenv.ProjectionDirectory, Path: "src"},
		{EntryID: "main", Kind: execenv.ProjectionFile, Path: "src/main.py", ContentVersion: "v1", Content: seededRef("other")},
	}}
	if _, err := j.Initialize(t.Context(), missing); !errors.Is(err, execenv.ErrMissingContent) {
		t.Fatalf("Initialize() with a body nobody has = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "stale.txt")); err != nil {
		t.Fatal("a refused initialization changed the workspace")
	}
	request := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{
		{EntryID: "src", Kind: execenv.ProjectionDirectory, Path: "src"},
		{EntryID: "main", Kind: execenv.ProjectionFile, Path: "src/main.py", ContentVersion: "v1", Content: seededRef("print('hi')")},
		{EntryID: "added", Kind: execenv.ProjectionFile, Path: "src/added.txt", ContentVersion: "v1", Content: &added},
	}}
	if _, err := j.Initialize(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"stale.txt", "src/old.py"} {
		if _, err := os.Stat(filepath.Join(home, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s survived initialization: %v", gone, err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(home, "src/added.txt")); err != nil || string(body) != "added" {
		t.Fatalf("uploaded file = %q, %v", body, err)
	}
	after, err := os.Stat(filepath.Join(home, "src/main.py"))
	if err != nil || !os.SameFile(seededInfo, after) || !after.ModTime().Equal(seededInfo.ModTime()) {
		t.Fatal("a seeded file that matched was rewritten")
	}
	if page, err := j.Observe(t.Context(), fence, 0, 128); err != nil || len(page.Observations) != 0 {
		t.Fatalf("Observe() after initialization = %+v, %v", page, err)
	}
}
