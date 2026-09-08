//go:build linux || darwin

package guesttree

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sudosylabs/execenv"
)

func capture(t *testing.T, home string, previous *Snapshot) *Snapshot {
	t.Helper()
	snap, err := Capture(home, previous)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(snap.Close)
	return snap
}
func write(t *testing.T, home, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func mkdir(t *testing.T, home, name string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(home, name), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryMoveUsesIdentityAndRetainsCapturedBytes(t *testing.T) {
	home := t.TempDir()
	mkdir(t, home, "dir")
	mkdir(t, home, "dir/nested")
	write(t, home, "dir/nested/file", "original")
	first := capture(t, home, nil)
	if err := os.Rename(filepath.Join(home, "dir"), filepath.Join(home, "moved")); err != nil {
		t.Fatal(err)
	}
	write(t, home, "moved/nested/file", "observed")
	second := capture(t, home, first)
	changes, err := Diff(first, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Operation != Move || changes[0].Path != "dir" || changes[0].Destination != "moved" || changes[1].Operation != WriteFile {
		t.Fatalf("semantic subtree move: %+v", changes)
	}
	for old, node := range first.Nodes {
		next := second.Nodes["moved"+old[len("dir"):]]
		if node.Identity != next.Identity {
			t.Fatalf("identity changed for %s", old)
		}
	}
	write(t, home, "moved/nested/file", "later live bytes")
	if string(changes[1].Data) != "observed" {
		t.Fatal("event reads later live bytes")
	}
	if string(first.Nodes["dir/nested/file"].Data) != "original" {
		t.Fatal("capture mutated prior baseline")
	}
	changes[1].Data[0] = 'X'
	if string(second.Nodes["moved/nested/file"].Data) != "observed" {
		t.Fatal("change bytes alias captured state")
	}
}

func TestSemanticCreateAndDeletes(t *testing.T) {
	home := t.TempDir()
	first := capture(t, home, nil)
	mkdir(t, home, "dir")
	mkdir(t, home, "dir/nested")
	write(t, home, "dir/nested/file", "a")
	write(t, home, "dir/keep", "b")
	second := capture(t, home, first)
	changes, err := Diff(first, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 4 || changes[0].Operation != CreateDirectory || changes[0].Path != "dir" {
		t.Fatalf("parent ordering: %+v", changes)
	}
	positions := map[string]int{}
	for i, c := range changes {
		positions[c.Path] = i
	}
	if positions["dir/nested"] >= positions["dir/nested/file"] {
		t.Fatal("file preceded directory")
	}
	if err := os.Remove(filepath.Join(home, "dir/nested/file")); err != nil {
		t.Fatal(err)
	}
	third := capture(t, home, second)
	changes, err = Diff(second, third)
	if err != nil || len(changes) != 1 || changes[0].Operation != Delete || changes[0].Path != "dir/nested/file" || changes[0].Recursive {
		t.Fatalf("ordinary descendant delete: %+v %v", changes, err)
	}
	if err := os.RemoveAll(filepath.Join(home, "dir")); err != nil {
		t.Fatal(err)
	}
	fourth := capture(t, home, third)
	changes, err = Diff(third, fourth)
	if err != nil || len(changes) != 1 || changes[0].Operation != Delete || changes[0].Kind != execenv.ProjectionDirectory || !changes[0].Recursive {
		t.Fatalf("recursive directory delete: %+v %v", changes, err)
	}
}

func TestCaptureRejectsUnsupportedTopology(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			write(t, home, "file", "original")
			before := capture(t, home, nil)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("file", filepath.Join(home, "other"))
			case "hardlink":
				err = os.Link(filepath.Join(home, "file"), filepath.Join(home, "other"))
			case "fifo":
				err = makeFIFO(filepath.Join(home, "other"))
			case "oversized":
				err = os.Truncate(filepath.Join(home, "file"), execenv.MaxFileBytes+1)
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := Capture(home, before)
			if !errors.Is(err, execenv.ErrLagged) || after != nil {
				t.Fatalf("unsupported tree became observation: %+v %v", after, err)
			}
			if string(before.Nodes["file"].Data) != "original" {
				t.Fatal("failed capture mutated baseline")
			}
		})
	}
}

func TestEqualContentDoesNotInventMove(t *testing.T) {
	home := t.TempDir()
	write(t, home, "old", "same bytes")
	before := capture(t, home, nil)
	if err := os.Remove(filepath.Join(home, "old")); err != nil {
		t.Fatal(err)
	}
	write(t, home, "new", "same bytes")
	after := capture(t, home, before)
	changes, err := Diff(before, after)
	if err != nil || len(changes) != 2 || changes[0].Operation != Delete || changes[1].Operation != CreateFile {
		t.Fatalf("hash guessed a move: %+v %v", changes, err)
	}
	if before.Nodes["old"].Identity == after.Nodes["new"].Identity {
		t.Fatal("deleted inode identity reused")
	}
}
