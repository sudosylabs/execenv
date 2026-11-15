package tree

import (
	"errors"
	"github.com/sudosylabs/execenv"
	"testing"
)

func TestDirectoryMovePreservesDescendantVersions(t *testing.T) {
	cur := Snapshot{"src": {Kind: KindDirectory}, "src/nested": {Kind: KindDirectory}, "src/nested/file": {Kind: KindFile, Version: "v7", Content: blob("saved")}}
	next, err := Apply(cur, Batch{Mutations: []Mutation{{Op: OpMove, From: "src", Path: "dst"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next["src/nested/file"]; ok {
		t.Fatal("old descendant retained")
	}
	if next["dst/nested/file"].Version != "v7" || next["dst/nested/file"].Content != blob("saved") {
		t.Fatal("descendant version/body lost")
	}
	if cur["src/nested/file"].Version != "v7" {
		t.Fatal("input mutated")
	}
	if _, err := Apply(next, Batch{Mutations: []Mutation{{Op: OpReplace, Path: "dst/nested/file", Kind: KindFile, Expected: "v7", Version: "v8", Content: blob("updated")}}}); err != nil {
		t.Fatalf("descendant version not usable: %v", err)
	}
}

func TestDirectoryDeleteRequiresExplicitRecursion(t *testing.T) {
	cur := Snapshot{"dir": {Kind: KindDirectory}, "dir/file": {Kind: KindFile, Version: "v1"}, "keep": {Kind: KindFile}}
	if _, err := Apply(cur, Batch{Mutations: []Mutation{{Op: OpDelete, Path: "dir"}}}); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("implicit recursive delete: %v", err)
	}
	next, err := Apply(cur, Batch{Mutations: []Mutation{{Op: OpDelete, Path: "dir", Recursive: true}}})
	if err != nil || len(next) != 1 {
		t.Fatalf("recursive delete: %v %+v", err, next)
	}
	next, err = Apply(cur, Batch{Mutations: []Mutation{{Op: OpDelete, Path: "dir/file", Expected: "v1"}}})
	if err != nil || len(next) != 2 {
		t.Fatalf("ordinary descendant delete: %v %+v", err, next)
	}
}

func TestSubtreeMoveAndLaterConflictLeaveOriginalIntact(t *testing.T) {
	cur := Snapshot{"dir": {Kind: KindDirectory}, "dir/file": {Kind: KindFile, Version: "v1"}, "occupied": {Kind: KindFile}}
	for _, mutations := range [][]Mutation{
		{{Op: OpMove, From: "dir", Path: "dir/nested"}},
		{{Op: OpMove, From: "dir", Path: "occupied/nested"}},
		{{Op: OpMove, From: "dir", Path: "new"}, {Op: OpReplace, Path: "new/file", Expected: "stale", Content: blob("bad")}},
	} {
		if _, err := Apply(cur, Batch{Mutations: mutations}); err == nil {
			t.Fatal("invalid transaction accepted")
		}
		if len(cur) != 3 || cur["dir/file"].Version != "v1" {
			t.Fatal("failed batch mutated input")
		}
	}
}
