package tree

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestFromTreeRequiresFileBodies(t *testing.T) {
	t.Parallel()
	next, err := FromTree(Tree{{Path: "a.txt", Kind: KindFile, Version: "v1", Content: blob("one")}, {Path: "empty", Kind: KindFile, Content: blob("")}})
	if err != nil {
		t.Fatal(err)
	}
	if empty, ok := next["empty"]; next["a.txt"].Content != blob("one") || !ok || empty.Content != blob("") {
		t.Fatalf("snapshot = %+v", next)
	}
	if _, err := FromTree(Tree{{Path: "a.txt", Kind: KindFile, Version: "v1"}}); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("missing body error = %v", err)
	}
}

func TestApplyConflictLeavesCallerToDropNext(t *testing.T) {
	t.Parallel()
	cur := Snapshot{
		"a.txt": {Kind: KindFile, Version: "v1", Content: blob("one")},
	}
	_, err := Apply(cur, Batch{Mutations: []Mutation{{
		Op:       OpReplace,
		Path:     "a.txt",
		Kind:     KindFile,
		Expected: "v9",
		Content:  blob("two"),
	}}})
	if !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if cur["a.txt"].Content != blob("one") {
		t.Fatal("Apply mutated current on conflict")
	}
}

func TestApplyCreateConflict(t *testing.T) {
	t.Parallel()
	cur := Snapshot{"a.txt": {Kind: KindFile, Content: blob("x")}}
	_, err := Apply(cur, Batch{Mutations: []Mutation{{
		Op:      OpCreate,
		Path:    "a.txt",
		Kind:    KindFile,
		Content: blob("y"),
	}}})
	if !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}
