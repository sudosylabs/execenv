package journal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/tree"
)

func TestApplyHomeFailureRestoresHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := rewriteHome(home, tree.Snapshot{"blocked": {Kind: execenv.KindFile, Version: "v1", Data: []byte("old")}}); err != nil {
		t.Fatal(err)
	}
	err := applyHome(home, execenv.Batch{Mutations: []execenv.Mutation{
		{Op: execenv.OpReplace, Path: "blocked", Kind: execenv.KindFile, Data: []byte("new")},
		{Op: execenv.OpCreate, Path: "blocked/nested.txt", Kind: execenv.KindFile, Data: []byte("nope")},
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
	if err := rewriteHome(home, tree.Snapshot{"a.txt": {Kind: execenv.KindFile, Version: "v1", Data: []byte("one")}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "guest.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyHome(home, execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpReplace, Path: "a.txt", Kind: execenv.KindFile, Data: []byte("two")}}}); err != nil {
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
