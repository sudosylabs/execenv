package agent

import (
	"crypto/sha256"
	"errors"
	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/tree"
	"github.com/sudosylabs/execenv/internal/watchlog"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectionDoesNotAcknowledgeUnrelatedGuestWrite(t *testing.T) {
	old := fileMeta{hash: sha256.Sum256([]byte("old"))}
	s := &server{snap: map[string]fileMeta{"guest": old, "projected": old}}
	s.projectSnap(execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpReplace, Path: "projected", Kind: execenv.KindFile, Data: []byte("saved")}}})
	current := map[string]fileMeta{"guest": {hash: sha256.Sum256([]byte("guest change"))}, "projected": {hash: sha256.Sum256([]byte("saved"))}}
	events := diffSnap(s.snap, current)
	if len(events) != 1 || events[0].Path != "guest" || events[0].Op != execenv.OpReplace {
		t.Fatalf("guest change lost or echo leaked: %+v", events)
	}
}

func TestProjectionMovesDescendantBaselinesWithoutSwallowingWrites(t *testing.T) {
	old := fileMeta{hash: sha256.Sum256([]byte("old"))}
	s := &server{snap: map[string]fileMeta{"dir": {dir: true}, "dir/file": old}}
	s.projectSnap(execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpMove, From: "dir", Path: "new"}}})
	current := map[string]fileMeta{"new": {dir: true}, "new/file": {hash: sha256.Sum256([]byte("guest change"))}}
	events := diffSnap(s.snap, current)
	if len(events) != 1 || events[0].Path != "new/file" || events[0].Op != execenv.OpReplace {
		t.Fatalf("descendant baseline lost: %+v", events)
	}
}

func TestApplyKeepsUnobservedGuestDiskChange(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"guest", "projected"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := &server{home: home, projected: tree.Snapshot{"guest": {Kind: execenv.KindFile, Version: "v1", Data: []byte("old")}, "projected": {Kind: execenv.KindFile, Version: "v1", Data: []byte("old")}}, log: watchlog.New(64), pollStop: make(chan struct{})}
	s.resetSnap()
	cursor := s.log.Current()
	if err := os.WriteFile(filepath.Join(home, "guest"), []byte("unobserved"), 0600); err != nil {
		t.Fatal(err)
	}
	extra, err := encodeExtra(applyArgs{Batch: execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpReplace, Path: "projected", Kind: execenv.KindFile, Expected: "v1", Version: "v2", Data: []byte("saved")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.apply(t.Context(), frame{Extra: extra}); err != nil {
		t.Fatal(err)
	}
	s.diffOnce()
	_, events, err := s.log.Since(cursor)
	if err != nil || len(events) != 1 || events[0].Path != "guest" || events[0].Op != execenv.OpReplace {
		t.Fatalf("unobserved guest change lost: %+v %v", events, err)
	}
	cursor = s.log.Current()
	s.diffOnce()
	_, events, err = s.log.Since(cursor)
	if err != nil || len(events) != 0 {
		t.Fatalf("guest change repeated: %+v %v", events, err)
	}
}

func TestApplyRefusesUnobservedGuestOverwriteBeforeChangingAnyPath(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "a"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "b"), []byte("guest edit"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &server{home: home, projected: tree.Snapshot{"a": {Kind: execenv.KindFile, Version: "v1", Data: []byte("saved")}, "b": {Kind: execenv.KindFile, Version: "v1", Data: []byte("saved")}}}
	extra, err := encodeExtra(applyArgs{Batch: execenv.Batch{Mutations: []execenv.Mutation{
		{Op: execenv.OpReplace, Path: "a", Kind: execenv.KindFile, Expected: "v1", Version: "v2", Data: []byte("new")},
		{Op: execenv.OpReplace, Path: "b", Kind: execenv.KindFile, Expected: "v1", Version: "v2", Data: []byte("new")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.apply(t.Context(), frame{Extra: extra}); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("guest overwrite accepted: %v", err)
	}
	for path, want := range map[string]string{"a": "saved", "b": "guest edit"} {
		body, err := os.ReadFile(filepath.Join(home, path))
		if err != nil || string(body) != want {
			t.Fatalf("partial mutation of %s: %q %v", path, body, err)
		}
	}
}

func TestApplyRefusesCreateOverGuestIdentity(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "guest"), []byte("unacknowledged"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &server{home: home, projected: make(tree.Snapshot)}
	extra, err := encodeExtra(applyArgs{Batch: execenv.Batch{Mutations: []execenv.Mutation{{Op: execenv.OpCreate, Path: "guest", Kind: execenv.KindFile, Version: "v1", Data: []byte("caller")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.apply(t.Context(), frame{Extra: extra}); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("guest identity overwritten: %v", err)
	}
}

func TestUnsupportedGuestTopologyInvalidatesCursorInsteadOfInventingDeletion(t *testing.T) {
	for _, kind := range []string{"symlink", "kind-change"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			file := filepath.Join(home, "file")
			if err := os.WriteFile(file, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			s := &server{home: home, log: watchlog.New(64), pollStop: make(chan struct{})}
			s.resetSnap()
			cursor := s.log.Current()
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink("elsewhere", file); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(file, 0700); err != nil {
					t.Fatal(err)
				}
			}
			s.diffOnce()
			if _, events, err := s.log.Since(cursor); !errors.Is(err, execenv.ErrLagged) || len(events) != 0 {
				t.Fatalf("unsupported topology synthesized events: %+v %v", events, err)
			}
		})
	}
}
