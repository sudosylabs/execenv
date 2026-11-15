package memory_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

func TestAttachEchoesBytes(t *testing.T) {
	t.Parallel()
	host, err := memory.New(memory.Config{
		Images:   []execenv.Image{"default"},
		Capacity: execenv.DefaultGrant.Times(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.AttachRequest{})
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

func TestConformance(t *testing.T) {
	execenvtest.Run(t, func(t *testing.T, cfg execenvtest.Config) execenv.Host {
		t.Helper()
		host, err := memory.New(memory.Config{
			Images:        []execenv.Image{"default", "alt"},
			Capacity:      execenv.DefaultGrant.Times(2),
			TerminalGrace: cfg.TerminalGrace,
		})
		if err != nil {
			t.Fatal(err)
		}
		return host
	})
}

func TestLossConformance(t *testing.T) {
	execenvtest.RunLoss(t, func(t *testing.T) execenvtest.Lossy {
		t.Helper()
		host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(2)})
		if err != nil {
			t.Fatal(err)
		}
		return execenvtest.Lossy{Host: host, Lose: func(t *testing.T, id execenv.ID) {
			if !host.Lose(id) {
				t.Fatalf("Lose(%s) found no live grant", id)
			}
		}}
	})
}
