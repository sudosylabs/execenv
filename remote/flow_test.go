package remote_test

import (
	"context"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

// floodHost serves grants whose processes never stop producing output.
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

func (floodEnv) AttachProcess(context.Context, string, map[execenv.Stream]uint64) (execenv.Attachment, error) {
	return &floodAttachment{done: make(chan struct{})}, nil
}

type floodAttachment struct {
	offset uint64
	done   chan struct{}
}

func (a *floodAttachment) Next(context.Context) (execenv.Output, error) {
	select {
	case <-a.done:
		return execenv.Output{}, execenv.ErrClosed
	default:
	}
	data := make([]byte, 32<<10)
	for i := range data {
		data[i] = 'x'
	}
	out := execenv.Output{Kind: execenv.OutputData, Stream: execenv.StreamPTY, Offset: a.offset, Data: data}
	a.offset += uint64(len(data))
	return out, nil
}

func (a *floodAttachment) Close() error {
	select {
	case <-a.done:
	default:
		close(a.done)
	}
	return nil
}

// An attachment nobody reads fills only its own stream window: calls and
// other attachments on the same connection keep moving.
func TestUnreadAttachmentDelaysNoOtherStreamOrCall(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(2)})
	if err != nil {
		t.Fatal(err)
	}
	host := serveInner(t, floodHost{inner})
	flood, err := host.Ensure(t.Context(), execenv.Spec{ID: "flood", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	stalled, err := flood.AttachProcess(t.Context(), "flood", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	// Let the flood fill every window between the host and this reader.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := host.Ready(ctx); err != nil {
		t.Fatalf("Ready behind a stalled attachment: %v", err)
	}
	other, err := host.Ensure(ctx, execenv.Spec{ID: "other", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	sh := execenvtest.StartShell(t, other, running(t, other), "sh")
	if err := sh.Type("echo moving"); err != nil {
		t.Fatal(err)
	}
	sh.Expect("moving")
	// The stalled reader still gets its output once it reads.
	if out, err := stalled.Next(ctx); err != nil || len(out.Data) == 0 {
		t.Fatalf("stalled Next() = %+v, %v", out.Kind, err)
	}
}
