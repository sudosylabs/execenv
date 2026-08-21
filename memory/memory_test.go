package memory_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

func TestAttachEchoesBytes(t *testing.T) {
	t.Parallel()
	host, err := memory.New(memory.Config{
		Images: []execenv.Image{"default"},
		Slots:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(term, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("ping")) {
		t.Fatal("memory PTY did not echo")
	}
}

func TestWatchResumesFromCursor(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := env.Watch(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.Cursor()
	if cursor == "" {
		t.Fatal("initial cursor is empty")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := host.WriteGuest(t.Context(), "grant-1", "later.txt", []byte("saved")); err != nil {
		t.Fatal(err)
	}
	resumed, err := env.Watch(t.Context(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	event, err := resumed.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if event.Path != "later.txt" || event.Cursor == "" || resumed.Cursor() != event.Cursor {
		t.Fatalf("resumed event = %+v cursor=%q", event, resumed.Cursor())
	}
}

func TestReplaceInvalidatesWatchCursor(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := env.Watch(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := obs.Cursor()
	if err := env.ReplaceTree(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := obs.Next(t.Context()); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("Next() after ReplaceTree error = %v, want ErrLagged", err)
	}
	if _, err := env.Watch(t.Context(), cursor); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("Watch(old cursor) error = %v, want ErrLagged", err)
	}
}

func TestConformance(t *testing.T) {
	execenvtest.Run(t, func(t *testing.T) execenv.Host {
		t.Helper()
		host, err := memory.New(memory.Config{
			Images: []execenv.Image{"default", "alt"},
			Slots:  2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return host
	})
}
