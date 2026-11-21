package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	guestv1 "github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1"
	"github.com/sudosylabs/execenv/internal/procs"
	"github.com/sudosylabs/execenv/internal/wire"
)

// Start spawns spec in the guest and returns the host's handle on it. Its
// output streams from the guest at once; the host's history keeps it.
func (c *Client) Start(ctx context.Context, spec execenv.ProcessSpec) (procs.Backend, error) {
	if err := c.ioGuard(); err != nil {
		return nil, execenv.Error("start", err)
	}
	cctx, cancel := c.call(ctx)
	resp, err := c.agent.Spawn(cctx, connect.NewRequest(&guestv1.SpawnRequest{Spec: wire.ProcessSpecTo(spec)}))
	cancel()
	if err != nil {
		return nil, c.failure("start", err)
	}
	var started time.Time
	if t := resp.Msg.GetStarted(); t != nil {
		started = t.AsTime()
	}
	return c.open(spec.Tag, int(resp.Msg.GetPid()), spec.PTY != nil, started, nil)
}

// GuestProcess is a process the guest holds, as a restarted host finds it.
type GuestProcess struct {
	Spec execenv.ProcessSpec
	Info execenv.ProcessInfo
	// First is the oldest offset of each stream the guest still holds.
	First map[execenv.Stream]uint64
}

// Processes lists what the guest holds, running or exited.
func (c *Client) Processes(ctx context.Context) ([]GuestProcess, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.ListProcesses(ctx, connect.NewRequest(&guestv1.ListProcessesRequest{}))
	if err != nil {
		return nil, c.failure("processes", err)
	}
	out := make([]GuestProcess, 0, len(resp.Msg.GetProcesses()))
	for _, gp := range resp.Msg.GetProcesses() {
		spec, ok := wire.ProcessSpecFrom(gp.GetSpec())
		if !ok {
			return nil, execenv.Error("processes", execenv.ErrUnavailable)
		}
		out = append(out, GuestProcess{Spec: spec, Info: wire.ProcessInfoFrom(gp.GetInfo()), First: wire.OffsetsFrom(gp.GetFirst())})
	}
	return out, nil
}

// Adopt reads on from a process the guest holds, from the oldest output it
// still has.
func (c *Client) Adopt(gp GuestProcess) (procs.Backend, error) {
	return c.open(gp.Spec.Tag, gp.Info.PID, gp.Spec.PTY != nil, gp.Info.Started, gp.First)
}

// open streams tag's output from the offsets in from.
func (c *Client) open(tag string, pid int, pty bool, started time.Time, from map[execenv.Stream]uint64) (procs.Backend, error) {
	stream, err := c.agent.Output(c.ctx, connect.NewRequest(&guestv1.OutputRequest{Tag: tag, From: wire.OffsetsTo(from)}))
	if err != nil {
		return nil, c.failure("output", err)
	}
	b := &guestBackend{client: c, tag: tag, pid: pid, started: started, stream: stream, done: make(chan struct{}), writers: map[execenv.Stream]*io.PipeWriter{}, readers: map[execenv.Stream]io.Reader{}}
	names := []execenv.Stream{execenv.StreamStdout, execenv.StreamStderr}
	if pty {
		names = []execenv.Stream{execenv.StreamPTY}
	}
	for _, name := range names {
		r, w := io.Pipe()
		b.readers[name], b.writers[name] = r, w
	}
	go b.receive()
	return b, nil
}

// guestBackend is one guest process as the host's manager sees it.
type guestBackend struct {
	client  *Client
	tag     string
	pid     int
	stream  *connect.ServerStreamForClient[guestv1.OutputResponse]
	readers map[execenv.Stream]io.Reader
	writers map[execenv.Stream]*io.PipeWriter
	done    chan struct{}
	exit    execenv.ExitStatus
	started time.Time
	ended   time.Time

	feedMu sync.Mutex
	feed   *connect.ClientStreamForClient[guestv1.FeedRequest, guestv1.FeedResponse]
	closed bool
}

var _ procs.Backend = (*guestBackend)(nil)

// receive moves output into the stream pipes, so the manager's pumps hold
// the guest back through flow control. A link that ends first is a kill.
func (b *guestBackend) receive() {
	defer b.stream.Close()
	exited := false
	for b.stream.Receive() {
		msg := b.stream.Msg()
		if exit := msg.GetExit(); exit != nil {
			b.exit, exited = wire.ExitFrom(exit), true
			if t := msg.GetEnded(); t != nil {
				b.ended = t.AsTime()
			}
			break
		}
		data := msg.GetData()
		if data == nil {
			continue
		}
		w, ok := b.writers[execenv.Stream(data.GetStream())]
		if !ok {
			continue
		}
		if _, err := w.Write(data.GetData()); err != nil {
			break
		}
	}
	if !exited {
		b.exit = execenv.ExitStatus{Signal: 9}
	}
	for _, w := range b.writers {
		_ = w.Close()
	}
	close(b.done)
}

func (b *guestBackend) Streams() map[execenv.Stream]io.Reader { return b.readers }

func (b *guestBackend) Write(p []byte) (int, error) {
	b.feedMu.Lock()
	defer b.feedMu.Unlock()
	if b.closed {
		return 0, execenv.ErrClosed
	}
	if b.feed == nil {
		feed := b.client.agent.Feed(b.client.ctx)
		if err := feed.Send(&guestv1.FeedRequest{Part: &guestv1.FeedRequest_Tag{Tag: b.tag}}); err != nil {
			_, _ = feed.CloseAndReceive()
			return 0, execenv.ErrClosed
		}
		b.feed = feed
	}
	for rest := p; len(rest) > 0; {
		n := min(len(rest), wire.ChunkSize)
		if err := b.feed.Send(&guestv1.FeedRequest{Part: &guestv1.FeedRequest_Data{Data: rest[:n]}}); err != nil {
			_, _ = b.feed.CloseAndReceive()
			b.feed = nil
			return len(p) - len(rest), execenv.ErrClosed
		}
		rest = rest[n:]
	}
	return len(p), nil
}

// CloseInput ends the feed, so every byte sent is written, then closes the
// process's input.
func (b *guestBackend) CloseInput() error {
	b.feedMu.Lock()
	b.closed = true
	if b.feed != nil {
		_, _ = b.feed.CloseAndReceive()
		b.feed = nil
	}
	b.feedMu.Unlock()
	ctx, cancel := b.client.call(context.Background())
	defer cancel()
	_, err := b.client.agent.CloseFeed(ctx, connect.NewRequest(&guestv1.ProcessRef{Tag: b.tag}))
	if errors.Is(b.client.failure("close_input", err), execenv.ErrClosed) {
		return nil
	}
	return b.client.failure("close_input", err)
}

func (b *guestBackend) Resize(ctx context.Context, w execenv.Window) error {
	ctx, cancel := b.client.call(ctx)
	defer cancel()
	_, err := b.client.agent.ResizeProcess(ctx, connect.NewRequest(&guestv1.ResizeRequest{Tag: b.tag, Window: wire.WindowTo(w)}))
	return b.client.failure("resize", err)
}

func (b *guestBackend) Screen(ctx context.Context) (execenv.Screen, error) {
	ctx, cancel := b.client.call(ctx)
	defer cancel()
	resp, err := b.client.agent.ProcessScreen(ctx, connect.NewRequest(&guestv1.ProcessScreenRequest{Tag: b.tag}))
	if err != nil {
		return execenv.Screen{}, b.client.failure("screen", err)
	}
	return wire.ScreenFrom(resp.Msg), nil
}

func (b *guestBackend) Signal(s execenv.Signal) error {
	ctx, cancel := b.client.call(context.Background())
	defer cancel()
	_, err := b.client.agent.SignalProcess(ctx, connect.NewRequest(&guestv1.SignalRequest{Tag: b.tag, Signal: string(s)}))
	// Only a response from the guest can confirm that the process exited.
	// A local link hangup also uses ErrClosed and must remain retryable.
	if errors.Is(wire.HostError("signal", err), execenv.ErrClosed) {
		return procs.ErrProcessExited
	}
	return b.client.failure("signal", err)
}

func (b *guestBackend) Wait() (execenv.ExitStatus, time.Time) {
	<-b.done
	return b.exit, b.ended
}

func (b *guestBackend) Started() time.Time { return b.started }

func (b *guestBackend) PID() int { return b.pid }
