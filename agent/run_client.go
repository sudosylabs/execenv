package agent

import (
	"context"
	"errors"
	"io"
	"sync"

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
	stream, err := c.agent.Output(c.ctx, connect.NewRequest(&guestv1.OutputRequest{Tag: spec.Tag}))
	if err != nil {
		return nil, c.failure("output", err)
	}
	b := &guestBackend{client: c, tag: spec.Tag, pid: int(resp.Msg.GetPid()), stream: stream, done: make(chan struct{}), writers: map[execenv.Stream]*io.PipeWriter{}, readers: map[execenv.Stream]io.Reader{}}
	names := []execenv.Stream{execenv.StreamStdout, execenv.StreamStderr}
	if spec.PTY != nil {
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

func (b *guestBackend) Signal(s execenv.Signal) error {
	ctx, cancel := b.client.call(context.Background())
	defer cancel()
	_, err := b.client.agent.SignalProcess(ctx, connect.NewRequest(&guestv1.SignalRequest{Tag: b.tag, Signal: string(s)}))
	err = b.client.failure("signal", err)
	if errors.Is(err, execenv.ErrClosed) {
		return nil
	}
	return err
}

func (b *guestBackend) Wait() execenv.ExitStatus {
	<-b.done
	return b.exit
}

func (b *guestBackend) PID() int { return b.pid }
