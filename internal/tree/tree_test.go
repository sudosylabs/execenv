package tree

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestFromTreeRequiresFileBodies(t *testing.T) {
	t.Parallel()
	next, err := FromTree(execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v1", Data: []byte("one")}, {Path: "empty", Kind: execenv.KindFile, Data: []byte{}}})
	if err != nil {
		t.Fatal(err)
	}
	if empty, ok := next["empty"]; string(next["a.txt"].Data) != "one" || !ok || len(empty.Data) != 0 {
		t.Fatalf("snapshot = %+v", next)
	}
	if _, err := FromTree(execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v1"}}); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("missing body error = %v", err)
	}
}

func TestApplyConflictLeavesCallerToDropNext(t *testing.T) {
	t.Parallel()
	cur := Snapshot{
		"a.txt": {Kind: execenv.KindFile, Version: "v1", Data: []byte("one")},
	}
	_, err := Apply(cur, execenv.Batch{Mutations: []execenv.Mutation{{
		Op:       execenv.OpReplace,
		Path:     "a.txt",
		Kind:     execenv.KindFile,
		Expected: "v9",
		Data:     []byte("two"),
	}}})
	if !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if string(cur["a.txt"].Data) != "one" {
		t.Fatal("Apply mutated current on conflict")
	}
}

func TestApplyCreateConflict(t *testing.T) {
	t.Parallel()
	cur := Snapshot{"a.txt": {Kind: execenv.KindFile, Data: []byte("x")}}
	_, err := Apply(cur, execenv.Batch{Mutations: []execenv.Mutation{{
		Op:   execenv.OpCreate,
		Path: "a.txt",
		Kind: execenv.KindFile,
		Data: []byte("y"),
	}}})
	if !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}
