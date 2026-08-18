package agent

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/sudosylabs/execenv"
)

// Client is the host side of one agent connection.
type Client struct {
	sess    *session
	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan frame
	term    *clientTerm
	obs     *clientObs
	closed  bool
}

// NewClient takes an already-dialed guest link. It does not occupy a grant.
func NewClient(conn net.Conn) *Client {
	c := &Client{
		sess:    newSession(conn),
		pending: make(map[uint64]chan frame),
	}
	go c.readLoop()
	return c
}

// Close hangs up the agent connection. The machine is not torn down.
func (c *Client) Close() error {
	c.Hangup(execenv.ErrClosed)
	return c.sess.close()
}

// Hangup fails the live PTY and Watch with err without closing the
// connection. Revoke uses this so a killed grant surfaces ErrRevoked
// rather than a generic close.
func (c *Client) Hangup(err error) {
	c.mu.Lock()
	c.closed = true
	term := c.term
	c.term = nil
	obs := c.obs
	c.obs = nil
	for seq, ch := range c.pending {
		ch <- frame{Seq: seq, Status: statusOf(err)}
		delete(c.pending, seq)
	}
	c.mu.Unlock()
	if term != nil {
		term.hangup(err)
	}
	if obs != nil {
		obs.fail(err)
	}
}

func (c *Client) readLoop() {
	for {
		f, err := c.sess.recv()
		if err != nil {
			c.failAll()
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
		case kindWatch:
			c.deliverWatch(f)
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
		c.term.hangup(execenv.ErrClosed)
		c.term = nil
	}
	if c.obs != nil {
		c.obs.fail(execenv.ErrClosed)
		c.obs = nil
	}
}

func (c *Client) deliverPty(f frame) {
	c.mu.Lock()
	term := c.term
	c.mu.Unlock()
	if term == nil {
		return
	}
	if f.Status != "" {
		term.hangup(errorFromStatus(f.Status))
		return
	}
	term.push(f.Extra)
}

func (c *Client) deliverWatch(f frame) {
	c.mu.Lock()
	obs := c.obs
	c.mu.Unlock()
	if obs == nil {
		return
	}
	if f.Status != "" {
		obs.fail(errorFromStatus(f.Status))
		return
	}
	var ev execenv.Event
	if err := decodeExtra(f.Extra, &ev); err != nil {
		obs.fail(execenv.ErrUnavailable)
		return
	}
	obs.push(ev)
}

func (c *Client) call(ctx context.Context, method string, extra []byte) ([]byte, error) {
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
	err := c.sess.send(frame{
		Seq:    seq,
		Kind:   kindRequest,
		Method: method,
		Extra:  extra,
	})
	if err != nil {
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

// ReplaceTree writes tree under Home.
func (c *Client) ReplaceTree(ctx context.Context, tree execenv.Tree) error {
	extra, err := encodeExtra(replaceArgs{Tree: tree})
	if err != nil {
		return execenv.Error("replace", err)
	}
	_, err = c.call(ctx, methodReplace, extra)
	return err
}

// Apply applies an incremental batch under Home. Guest files the batch
// does not name are left alone.
func (c *Client) Apply(ctx context.Context, batch execenv.Batch) error {
	extra, err := encodeExtra(applyArgs{Batch: batch})
	if err != nil {
		return execenv.Error("apply", err)
	}
	_, err = c.call(ctx, methodApply, extra)
	return err
}

// Open reads a file from Home, including files the guest created.
func (c *Client) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	extra, err := encodeExtra(openArgs{Path: path})
	if err != nil {
		return nil, execenv.Error("open", err)
	}
	out, err := c.call(ctx, methodOpen, extra)
	if err != nil {
		return nil, err
	}
	var body []byte
	if err := decodeExtra(out, &body); err != nil {
		return nil, execenv.Error("open", execenv.ErrUnavailable)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// Watch streams guest-originated changes under Home.
func (c *Client) Watch(ctx context.Context) (execenv.Observation, error) {
	if _, err := c.call(ctx, methodWatch, nil); err != nil {
		return nil, err
	}
	obs := newClientObs(c)
	c.mu.Lock()
	c.obs = obs
	c.mu.Unlock()
	return obs, nil
}

// Attach starts a login shell with cwd at Home.
func (c *Client) Attach(ctx context.Context, win execenv.Window) (execenv.Terminal, error) {
	extra, err := encodeExtra(attachArgs{Window: win})
	if err != nil {
		return nil, execenv.Error("attach", err)
	}
	if _, err := c.call(ctx, methodAttach, extra); err != nil {
		return nil, err
	}
	term := newClientTerm(c)
	c.mu.Lock()
	c.term = term
	c.mu.Unlock()
	return term, nil
}

// Freeze stops agent I/O. Home and the connection stay.
func (c *Client) Freeze(ctx context.Context) error {
	_, err := c.call(ctx, methodFreeze, nil)
	c.mu.Lock()
	term := c.term
	c.term = nil
	c.mu.Unlock()
	if term != nil {
		term.hangup(execenv.ErrFrozen)
	}
	return err
}

// Thaw allows Attach and tree I/O again.
func (c *Client) Thaw(ctx context.Context) error {
	_, err := c.call(ctx, methodThaw, nil)
	return err
}

type clientTerm struct {
	client *Client
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	dead   error
}

func newClientTerm(c *Client) *clientTerm {
	t := &clientTerm{client: c}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *clientTerm) push(p []byte) {
	t.mu.Lock()
	if !t.closed && t.dead == nil {
		t.buf = append(t.buf, p...)
		t.cond.Broadcast()
	}
	t.mu.Unlock()
}

func (t *clientTerm) hangup(err error) {
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

func (t *clientTerm) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.buf) == 0 && !t.closed && t.dead == nil {
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

func (t *clientTerm) Write(p []byte) (int, error) {
	t.mu.Lock()
	if t.closed || t.dead != nil {
		err := t.dead
		if err == nil {
			err = execenv.ErrClosed
		}
		t.mu.Unlock()
		return 0, execenv.Error("write", err)
	}
	t.mu.Unlock()
	err := t.client.sess.send(frame{
		Kind:  kindPty,
		Extra: append([]byte(nil), p...),
	})
	if err != nil {
		return 0, execenv.Error("write", execenv.ErrClosed)
	}
	return len(p), nil
}

func (t *clientTerm) Resize(ctx context.Context, win execenv.Window) error {
	extra, err := encodeExtra(resizeArgs{Window: win})
	if err != nil {
		return execenv.Error("resize", err)
	}
	_, err = t.client.call(ctx, methodResize, extra)
	return err
}

func (t *clientTerm) Close() error {
	t.mu.Lock()
	t.closed = true
	t.cond.Broadcast()
	t.mu.Unlock()
	_, err := t.client.call(context.Background(), methodDetach, nil)
	t.client.mu.Lock()
	if t.client.term == t {
		t.client.term = nil
	}
	t.client.mu.Unlock()
	return err
}

type clientObs struct {
	client *Client
	events chan execenv.Event
	errc   chan error
	once   sync.Once
}

func newClientObs(c *Client) *clientObs {
	return &clientObs{
		client: c,
		events: make(chan execenv.Event, watchBuffer),
		errc:   make(chan error, 1),
	}
}

func (o *clientObs) push(ev execenv.Event) {
	select {
	case o.events <- ev:
	default:
		o.fail(execenv.ErrLagged)
	}
}

func (o *clientObs) fail(err error) {
	o.once.Do(func() {
		o.errc <- err
		close(o.events)
	})
}

func (o *clientObs) Next(ctx context.Context) (execenv.Event, error) {
	select {
	case <-ctx.Done():
		return execenv.Event{}, execenv.Error("watch", ctx.Err())
	case ev, ok := <-o.events:
		if ok {
			return ev, nil
		}
	}
	select {
	case err := <-o.errc:
		return execenv.Event{}, execenv.Error("watch", err)
	default:
		return execenv.Event{}, execenv.Error("watch", execenv.ErrClosed)
	}
}

func (o *clientObs) Close() error {
	o.fail(execenv.ErrClosed)
	_, err := o.client.call(context.Background(), methodUnwatch, nil)
	o.client.mu.Lock()
	if o.client.obs == o {
		o.client.obs = nil
	}
	o.client.mu.Unlock()
	return err
}
