package tree

import (
	"errors"
	"github.com/sudosylabs/execenv"
	"testing"
)

func TestDirectoryMovePreservesDescendantVersions(t *testing.T) {
	cur := Snapshot{"src": {Kind: execenv.KindDirectory}, "src/nested": {Kind: execenv.KindDirectory}, "src/nested/file": {Kind: execenv.KindFile, Version: "v7", Data: []byte("saved")}}
	next, err := Apply(cur, execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpMove, From: "src", Path: "dst"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next["src/nested/file"]; ok {
		t.Fatal("old descendant retained")
	}
	if next["dst/nested/file"].Version != "v7" || string(next["dst/nested/file"].Data) != "saved" {
		t.Fatal("descendant version/body lost")
	}
	if cur["src/nested/file"].Version != "v7" {
		t.Fatal("input mutated")
	}
	if _, err := Apply(next, execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpReplace, Path: "dst/nested/file", Kind: execenv.KindFile, Expected: "v7", Version: "v8", Data: []byte("updated")}}}); err != nil {
		t.Fatalf("descendant version not usable: %v", err)
	}
}

func TestDirectoryDeleteRequiresExplicitRecursion(t *testing.T) {
	cur := Snapshot{"dir": {Kind: execenv.KindDirectory}, "dir/file": {Kind: execenv.KindFile, Version: "v1"}, "keep": {Kind: execenv.KindFile}}
	if _, err := Apply(cur, execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpDelete, Path: "dir"}}}); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("implicit recursive delete: %v", err)
	}
	next, err := Apply(cur, execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpDelete, Path: "dir", Recursive: true}}})
	if err != nil || len(next) != 1 {
		t.Fatalf("recursive delete: %v %+v", err, next)
	}
	next, err = Apply(cur, execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpDelete, Path: "dir/file", Expected: "v1"}}})
	if err != nil || len(next) != 2 {
		t.Fatalf("ordinary descendant delete: %v %+v", err, next)
	}
}

func TestSubtreeMoveAndLaterConflictLeaveOriginalIntact(t *testing.T) {
	cur := Snapshot{"dir": {Kind: execenv.KindDirectory}, "dir/file": {Kind: execenv.KindFile, Version: "v1"}, "occupied": {Kind: execenv.KindFile}}
	for _, mutations := range [][]execenv.Mutation{
		{{Op: execenv.OpMove, From: "dir", Path: "dir/nested"}},
		{{Op: execenv.OpMove, From: "dir", Path: "occupied/nested"}},
		{{Op: execenv.OpMove, From: "dir", Path: "new"}, {Op: execenv.OpReplace, Path: "new/file", Expected: "stale", Data: []byte("bad")}},
	} {
		if _, err := Apply(cur, execenv.Batch{Mutations: mutations}); err == nil {
			t.Fatal("invalid transaction accepted")
		}
		if len(cur) != 3 || cur["dir/file"].Version != "v1" {
			t.Fatal("failed batch mutated input")
		}
	}
}
