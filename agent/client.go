package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/mux"
	"github.com/sudosylabs/execenv/internal/processes"
)

// Client is the host side of one agent connection.
type Client struct {
	sess    *mux.Session
	seq     atomic.Uint64
	stream  atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan frame
	term    *PTY
	closed  bool
	frozen  bool
	// done closes once the guest link has failed or been closed.
	done chan struct{}
	// processes is the guest's process log, retained on the host until
	// the caller acknowledges it.
	processes *processes.Log
}

// NewClient takes an already-dialed guest link. It does not occupy a grant.
func NewClient(conn net.Conn) *Client {
	c := &Client{
		sess:      newSession(conn),
		pending:   make(map[uint64]chan frame),
		processes: processes.New(),
		done:      make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Verify rejects a guest helper that speaks another protocol revision,
// cannot serve the native journal, or is not recording the workload's
// processes. Host readiness must not infer capability from its own binary.
func (c *Client) Verify(ctx context.Context) error {
	out, err := c.call(ctx, methodHello, nil)
	if err != nil {
		return execenv.Error("agent protocol", execenv.ErrUnavailable)
	}
	var result helloResult
	if err := decodeExtra(out, &result); err != nil || result.Version != protocolVersion || !result.Journal || !result.Processes {
		return execenv.Error("agent protocol", execenv.ErrUnavailable)
	}
	return nil
}

// Done is closed once the guest link is gone, whether the guest end failed
// or Close hung it up. The link cannot be re-established: a new link would
// be a new helper session without this one's terminal or process log.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close hangs up the agent connection. The machine is not torn down.
func (c *Client) Close() error {
	c.Hangup(execenv.ErrClosed)
	return c.sess.Close()
}

// Hangup fails the live PTY and pending calls with err without closing the
// connection. Revoke uses this so a killed grant surfaces ErrRevoked rather
// than a generic close.
func (c *Client) Hangup(err error) {
	c.mu.Lock()
	c.closed = true
	term := c.term
	c.term = nil
	for seq, ch := range c.pending {
		ch <- frame{Seq: seq, Status: statusOf(err)}
		delete(c.pending, seq)
	}
	c.mu.Unlock()
	if term != nil {
		term.hangup(err)
	}
}

func (c *Client) readLoop() {
	for {
		f, err := c.sess.Recv()
		if err != nil {
			c.failAll()
			close(c.done)
			return
		}
		switch f.Kind {
		case kindResponse:
			c.mu.Lock()
			ch := c.pending[f.Seq]
			delete(c.pending, f.Seq)
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case kindPty:
			c.deliverPty(f)
		case mux.KindEvent:
			c.deliverProcesses(f)
		}
	}
}

func (c *Client) failAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for seq, ch := range c.pending {
		ch <- frame{Seq: seq, Status: "unavailable"}
		delete(c.pending, seq)
	}
	if c.term != nil {
		c.term.hangup(execenv.ErrConnection)
		c.term = nil
	}
}

func (c *Client) deliverPty(f frame) {
	c.mu.Lock()
	term := c.term
	c.mu.Unlock()
	if term == nil || term.stream != f.Stream {
		return
	}
	if f.Status != "" {
		term.hangup(errorFromStatus(f.Status))
		return
	}
	// The guest ends a stream with one empty frame when the shell exits.
	if len(f.Extra) == 0 {
		c.mu.Lock()
		if c.term == term {
			c.term = nil
		}
		c.mu.Unlock()
		term.finish()
		return
	}
	term.push(f.Extra)
}

func (c *Client) call(ctx context.Context, method string, extra []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error(method, err)
	}
	seq := c.seq.Add(1)
	ch := make(chan frame, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, execenv.Error(method, execenv.ErrUnavailable)
	}
	c.pending[seq] = ch
	c.mu.Unlock()
	request := frame{
		Seq:    seq,
		Kind:   kindRequest,
		Method: method,
		Extra:  extra,
	}
	if deadline, ok := ctx.Deadline(); ok {
		request.Deadline = deadline.UnixNano()
	}
	err := c.sess.Send(request)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		if errors.Is(err, execenv.ErrTooLarge) || errors.Is(err, execenv.ErrInvalid) {
			return nil, execenv.Error(method, err)
		}
		return nil, execenv.Error(method, execenv.ErrUnavailable)
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		return nil, execenv.Error(method, ctx.Err())
	case f := <-ch:
		if f.Status != "" {
			return nil, execenv.Error(method, errorFromStatus(f.Status))
		}
		return f.Extra, nil
	}
}

// Attach starts a login shell with cwd at Home. The guest ends that shell
// when the PTY is closed; the host keeps terminal sessions above this.
func (c *Client) Attach(ctx context.Context, win execenv.Window) (*PTY, error) {
	if err := c.ioGuard(); err != nil {
		return nil, err
	}
	stream := c.stream.Add(1)
	extra, err := encodeExtra(attachArgs{Window: win, Stream: stream})
	if err != nil {
		return nil, execenv.Error("attach", err)
	}
	term := newClientTerm(c, stream)
	c.mu.Lock()
	if c.term != nil {
		c.mu.Unlock()
		return nil, execenv.Error("attach", execenv.ErrBusy)
	}
	c.term = term
	c.mu.Unlock()
	if _, err := c.call(ctx, methodAttach, extra); err != nil {
		c.mu.Lock()
		if c.term == term {
			c.term = nil
		}
		c.mu.Unlock()
		term.hangup(err)
		return nil, err
	}
	return term, nil
}

// Quiesce stops agent I/O. Home and the connection stay. The host calls it
// from fenced control before it pauses the machine.
func (c *Client) Quiesce(ctx context.Context) error {
	c.mu.Lock()
	c.frozen = true
	c.mu.Unlock()
	_, err := c.call(ctx, methodFreeze, nil)
	return err
}

// Reopen reopens I/O only after the guest acknowledges recovery.
func (c *Client) Reopen(ctx context.Context) error {
	_, err := c.call(ctx, methodThaw, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	term := c.term
	c.mu.Unlock()
	if term != nil {
		term.mu.Lock()
		closed := term.closed
		term.mu.Unlock()
		if closed {
			extra, err := encodeExtra(streamArgs{Stream: term.stream})
			if err != nil {
				return err
			}
			if _, err := c.call(ctx, methodDetach, extra); err != nil {
				return err
			}
			c.mu.Lock()
			if c.term == term {
				c.term = nil
			}
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	c.frozen = false
	c.mu.Unlock()
	return nil
}

func (c *Client) ioGuard() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.frozen {
		return execenv.ErrFrozen
	}
	return nil
}

// PTY is the host end of the guest's one login shell. Read returns io.EOF
// once the shell exits and its output is drained, or after Close.
type PTY struct {
	client *Client
	stream uint64
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	ended  bool
	dead   error
}

func newClientTerm(c *Client, stream uint64) *PTY {
	t := &PTY{client: c, stream: stream}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *PTY) push(p []byte) {
	t.mu.Lock()
	if !t.closed && t.dead == nil {
		t.buf = append(t.buf, p...)
		t.cond.Broadcast()
	}
	t.mu.Unlock()
}

func (t *PTY) finish() {
	t.mu.Lock()
	t.ended = true
	t.cond.Broadcast()
	t.mu.Unlock()
}

func (t *PTY) hangup(err error) {
	t.mu.Lock()
	if t.dead == nil {
		if err == nil {
			err = execenv.ErrClosed
		}
		t.dead = err
		t.cond.Broadcast()
	}
	t.mu.Unlock()
}

func (t *PTY) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.buf) == 0 && !t.closed && !t.ended && t.dead == nil {
		t.cond.Wait()
	}
	if t.dead != nil && len(t.buf) == 0 {
		return 0, execenv.Error("read", t.dead)
	}
	if len(t.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

func (t *PTY) Write(p []byte) (int, error) {
	if err := t.client.ioGuard(); err != nil {
		return 0, err
	}
	t.mu.Lock()
	if t.closed || t.ended || t.dead != nil {
		err := t.dead
		if err == nil {
			err = execenv.ErrClosed
		}
		t.mu.Unlock()
		return 0, execenv.Error("write", err)
	}
	t.mu.Unlock()
	err := t.client.sess.Send(frame{
		Kind:   kindPty,
		Stream: t.stream,
		Extra:  append([]byte(nil), p...),
	})
	if err != nil {
		if errors.Is(err, execenv.ErrTooLarge) {
			return 0, execenv.Error("write", err)
		}
		return 0, execenv.Error("write", execenv.ErrClosed)
	}
	return len(p), nil
}

func (t *PTY) Resize(ctx context.Context, win execenv.Window) error {
	if err := t.client.ioGuard(); err != nil {
		return err
	}
	extra, err := encodeExtra(resizeArgs{Window: win, Stream: t.stream})
	if err != nil {
		return execenv.Error("resize", err)
	}
	_, err = t.client.call(ctx, methodResize, extra)
	return err
}

// Close hangs the guest shell up.
func (t *PTY) Close() error {
	t.mu.Lock()
	t.closed = true
	t.cond.Broadcast()
	t.mu.Unlock()
	// A paused guest cannot acknowledge RPC. Keep the exact closed stream
	// attached internally; Reopen drains its deferred detach before reopening I/O.
	t.client.mu.Lock()
	frozen := t.client.frozen
	t.client.mu.Unlock()
	if frozen {
		return nil
	}

	extra, encodeErr := encodeExtra(streamArgs{Stream: t.stream})
	if encodeErr != nil {
		return execenv.Error("detach", encodeErr)
	}
	_, err := t.client.call(context.Background(), methodDetach, extra)
	t.client.mu.Lock()
	if t.client.term == t {
		t.client.term = nil
	}
	t.client.mu.Unlock()
	return err
}
