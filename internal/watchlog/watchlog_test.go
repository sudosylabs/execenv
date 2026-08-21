package watchlog

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestSinceReplaysAndRejectsEvictedCursor(t *testing.T) {
	log := New(2)
	initial := log.Current()
	first := log.Append(execenv.Event{Op: execenv.OpCreate, Path: "one"})
	log.Append(execenv.Event{Op: execenv.OpCreate, Path: "two"})
	log.Append(execenv.Event{Op: execenv.OpCreate, Path: "three"})

	_, replay, err := log.Since(first.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 2 || replay[0].Path != "two" || replay[1].Path != "three" {
		t.Fatalf("replay = %+v", replay)
	}
	if _, _, err := log.Since(initial); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("Since(evicted) error = %v, want ErrLagged", err)
	}
}

func TestResetInvalidatesOldCursor(t *testing.T) {
	log := New(2)
	cursor := log.Current()
	log.Reset()
	if _, _, err := log.Since(cursor); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("Since(old generation) error = %v, want ErrLagged", err)
	}
}
