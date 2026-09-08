package guesttree

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestMoveOutBeforeRecursiveDelete(t *testing.T) {
	directory := Node{Identity: "dir-id", Kind: execenv.ProjectionDirectory}
	survivor := Node{Identity: "file-id", Kind: execenv.ProjectionFile, Data: []byte("keep")}
	removed := Node{Identity: "other-id", Kind: execenv.ProjectionFile, Data: []byte("remove")}
	before := &Snapshot{Nodes: map[string]Node{"dir": directory, "dir/survivor": survivor, "dir/removed": removed}}
	after := &Snapshot{Nodes: map[string]Node{"survivor": survivor}}
	changes, err := Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	moved, deleted := -1, -1
	for i, c := range changes {
		if c.Operation == Move && c.Identity == "file-id" {
			moved = i
		}
		if c.Operation == Delete && c.Identity == "dir-id" {
			deleted = i
		}
	}
	if moved < 0 || deleted <= moved {
		t.Fatalf("directory deletion swallowed surviving identity: %+v", changes)
	}
}
func TestIdentityCyclesAndKindChangesRequireResynchronization(t *testing.T) {
	a := Node{Identity: "a", Kind: execenv.ProjectionFile}
	b := Node{Identity: "b", Kind: execenv.ProjectionFile}
	before := &Snapshot{Nodes: map[string]Node{"a": a, "b": b}}
	for _, after := range []*Snapshot{
		{Nodes: map[string]Node{"a": b, "b": a}},
		{Nodes: map[string]Node{"a": {Identity: "c", Kind: execenv.ProjectionDirectory}, "b": b}},
		{Nodes: map[string]Node{"a": a, "b": a}},
	} {
		if changes, err := Diff(before, after); !errors.Is(err, execenv.ErrLagged) || changes != nil {
			t.Fatalf("invented unsupported operations: %+v %v", changes, err)
		}
	}
}
