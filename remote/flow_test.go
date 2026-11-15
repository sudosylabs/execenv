package remote_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

// floodHost serves grants whose terminal never stops producing output.
type floodHost struct{ execenv.Host }

func (h floodHost) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	env, err := h.Host.Ensure(ctx, spec)
	if err != nil {
		return nil, err
	}
	if spec.ID != "flood" {
		return env, nil
	}
	return floodEnv{env}, nil
}

type floodEnv struct{ execenv.Env }

func (floodEnv) Attach(context.Context, execenv.AttachRequest) (execenv.Terminal, error) {
	return &floodTerminal{done: make(chan struct{})}, nil
}

type floodTerminal struct {
	offset uint64
	done   chan struct{}
}

func (t *floodTerminal) ID() execenv.TerminalID { return "flood" }
func (t *floodTerminal) Start() uint64          { return 0 }
func (t *floodTerminal) Offset() uint64         { return t.offset }

func (t *floodTerminal) Read(p []byte) (int, error) {
	select {
	case <-t.done:
		return 0, execenv.ErrClosed
	default:
	}
	for i := range p {
		p[i] = 'x'
	}
	t.offset += uint64(len(p))
	return len(p), nil
}

func (t *floodTerminal) Write(p []byte) (int, error)                  { return len(p), nil }
func (t *floodTerminal) Resize(context.Context, execenv.Window) error { return nil }
func (t *floodTerminal) Hangup(context.Context) error                 { return nil }
func (t *floodTerminal) Close() error {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	return nil
}

// A terminal whose output nobody reads fills only its own stream window:
// calls and other terminals on the same connection keep moving.
func TestUnreadTerminalDelaysNoOtherStreamOrCall(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	host := serveInner(t, floodHost{inner})
	flood, err := host.Ensure(t.Context(), execenv.Spec{ID: "flood", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	stalled, err := flood.Attach(t.Context(), execenv.AttachRequest{Window: execenv.Window{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	// Let the flood fill every window between the host and this reader.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := host.Ready(ctx); err != nil {
		t.Fatalf("Ready behind a stalled terminal: %v", err)
	}
	other, err := host.Ensure(ctx, execenv.Spec{ID: "other", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	term, err := other.Attach(ctx, execenv.AttachRequest{Window: execenv.Window{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatalf("Attach behind a stalled terminal: %v", err)
	}
	defer term.Close()
	if _, err := term.Write([]byte("echo moving\n")); err != nil {
		t.Fatal(err)
	}
	if !readUntil(t, term, "moving") {
		t.Fatal("second terminal made no progress")
	}
	// The stalled reader still gets its output once it reads.
	buf := make([]byte, 1024)
	if n, err := io.ReadAtLeast(stalled, buf, 1); err != nil || n == 0 {
		t.Fatalf("stalled terminal Read = %d, %v", n, err)
	}
}

func readUntil(t *testing.T, term execenv.Terminal, marker string) bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() {
		var seen []byte
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			seen = append(seen, buf[:n]...)
			if containsString(seen, marker) {
				done <- true
				return
			}
			if err != nil {
				done <- false
				return
			}
		}
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(2 * time.Second):
		return false
	}
}

func containsString(haystack []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
