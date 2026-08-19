package tree

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestReplaceKeepsVersionBody(t *testing.T) {
	t.Parallel()
	cur := Snapshot{
		"a.txt": {Kind: execenv.KindFile, Version: "v1", Data: []byte("one")},
	}
	next, err := Replace(cur, execenv.Tree{{
		Path: "a.txt", Kind: execenv.KindFile, Version: "v1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if string(next["a.txt"].Data) != "one" {
		t.Fatalf("body = %q", next["a.txt"].Data)
	}
	if _, err := Replace(cur, execenv.Tree{{
		Path: "a.txt", Kind: execenv.KindFile, Version: "v2",
	}}); !errors.Is(err, execenv.ErrInvalid) {
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
