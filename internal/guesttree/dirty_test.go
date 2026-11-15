package guesttree

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// After each change, a capture of just the paths it touched equals a walk
// of the whole tree: the same nodes, identities and contents, and so the
// same changes between captures.
func TestCaptureDirtyMatchesAWholeWalk(t *testing.T) {
	home := t.TempDir()
	at := func(name string) string { return filepath.Join(home, filepath.FromSlash(name)) }
	type step struct {
		name  string
		do    func() error
		dirty []string
	}
	steps := []step{
		{"create a file", func() error { return os.WriteFile(at("a.txt"), []byte("one"), 0o600) }, []string{"a.txt"}},
		{"make a tree", func() error {
			if err := os.MkdirAll(at("src/pkg"), 0o700); err != nil {
				return err
			}
			return os.WriteFile(at("src/pkg/x.go"), []byte("package x"), 0o600)
		}, []string{"src", "src/pkg", "src/pkg/x.go"}},
		{"write a file", func() error { return os.WriteFile(at("a.txt"), []byte("two, longer"), 0o600) }, []string{"a.txt"}},
		{"rename a file", func() error { return os.Rename(at("a.txt"), at("src/a.txt")) }, []string{"a.txt", "src/a.txt"}},
		{"rename a directory", func() error { return os.Rename(at("src/pkg"), at("lib")) }, []string{"src/pkg", "lib"}},
		{"write inside the moved one", func() error { return os.WriteFile(at("lib/x.go"), []byte("package y"), 0o600) }, []string{"lib/x.go"}},
		{"add beside", func() error { return os.WriteFile(at("lib/y.go"), []byte("package y"), 0o600) }, []string{"lib/y.go"}},
		{"remove a file", func() error { return os.Remove(at("src/a.txt")) }, []string{"src/a.txt"}},
		{"remove a tree", func() error { return os.RemoveAll(at("lib")) }, []string{"lib/x.go", "lib/y.go", "lib"}},
	}
	whole, err := Capture(home, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	part := whole
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		nextWhole, err := Capture(home, whole, nil)
		if err != nil {
			t.Fatalf("%s: Capture: %v", s.name, err)
		}
		nextPart, err := CaptureDirty(home, part, nil, s.dirty)
		if err != nil {
			t.Fatalf("%s: CaptureDirty: %v", s.name, err)
		}
		if !sameNodes(nextWhole, nextPart) {
			t.Fatalf("%s: dirty capture %v, whole walk %v", s.name, names(nextPart), names(nextWhole))
		}
		changesWhole, err := Diff(whole, nextWhole)
		if err != nil {
			t.Fatal(err)
		}
		changesPart, err := Diff(part, nextPart)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.EqualFunc(changesWhole, changesPart, func(a, b Change) bool {
			return a.Operation == b.Operation && a.Path == b.Path && a.Destination == b.Destination && a.Kind == b.Kind && a.Content == b.Content
		}) {
			t.Fatalf("%s: dirty changes %+v, whole %+v", s.name, changesPart, changesWhole)
		}
		whole, part = nextWhole, nextPart
	}
}

// A path under a directory the last capture did not hold makes a whole
// walk, so an event lost before it cannot hide the directory.
func TestCaptureDirtyWalksWhenAParentIsUnknown(t *testing.T) {
	home := t.TempDir()
	before, err := Capture(home, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "new/deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "new/deep/f"), []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := CaptureDirty(home, before, nil, []string{"new/deep/f"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Nodes["new"]; !ok {
		t.Fatalf("lost the unreported directory: %v", names(after))
	}
}

func sameNodes(a, b *Snapshot) bool {
	if len(a.Nodes) != len(b.Nodes) {
		return false
	}
	for name, na := range a.Nodes {
		nb, ok := b.Nodes[name]
		if !ok || na.Kind != nb.Kind || na.Content != nb.Content || na.key != nb.key {
			return false
		}
	}
	return true
}

func names(s *Snapshot) []string {
	out := make([]string, 0, len(s.Nodes))
	for name := range s.Nodes {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
