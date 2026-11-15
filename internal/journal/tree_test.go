package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv/internal/content"

	"github.com/sudosylabs/execenv/internal/tree"
)

func TestApplyHomeFailureRestoresHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store := testStore(t)
	if err := os.WriteFile(filepath.Join(home, "blocked"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := applyHome(store, home, tree.Batch{Mutations: []tree.Mutation{
		{Op: tree.OpReplace, Path: "blocked", Kind: tree.KindFile, Content: put(t, store, "new")},
		{Op: tree.OpCreate, Path: "blocked/nested.txt", Kind: tree.KindFile, Content: put(t, store, "nope")},
	}})
	if err == nil {
		t.Fatal("applyHome succeeded with a file parent")
	}
	got, err := os.ReadFile(filepath.Join(home, "blocked"))
	if err != nil || string(got) != "old" {
		t.Fatalf("home after failed apply = %q %v, want old", got, err)
	}
}

func TestApplyHomeReplaceDoesNotLeaveStash(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store := testStore(t)
	if err := os.WriteFile(filepath.Join(home, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "guest.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyHome(store, home, tree.Batch{Mutations: []tree.Mutation{{Op: tree.OpReplace, Path: "a.txt", Kind: tree.KindFile, Content: put(t, store, "two")}}}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("home entries = %d, want a.txt and the untouched guest.txt", len(entries))
	}
	if got, err := os.ReadFile(filepath.Join(home, "guest.txt")); err != nil || string(got) != "hi" {
		t.Fatalf("guest file after apply = %q %v", got, err)
	}
}

// put stores body and names it.
func put(t *testing.T, store *content.Store, body string) content.Blob {
	t.Helper()
	blob, err := store.Put(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return blob
}
