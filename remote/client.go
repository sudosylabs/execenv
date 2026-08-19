package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/mux"
)

var _ execenv.Host = (*Client)(nil)

// Client is a Host that speaks the execenv contract over one connection.
type Client struct {
	cfg     Config
	sess    *mux.Session
	seq     atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan frame
	terms   map[execenv.ID]*remoteTerminal
	obs     map[execenv.ID]*remoteObservation
	closed  bool
}

// New dials cfg.Address. The constructor is inert aside from the dial and
// the auth frame: it does not occupy grants.
func New(cfg Config) (*Client, error) {
	if err := validateClient(cfg); err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: timeoutOrDefault(cfg.Timeout)}
	var conn net.Conn
	var err error
	if cfg.Security == SecurityTLS {
		tlsCfg := cfg.TLS.Clone()
		if tlsCfg.ServerName == "" {
			tlsCfg.ServerName = cfg.ServerName
		}
		conn, err = tls.DialWithDialer(&dialer, "tcp", cfg.Address, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", cfg.Address)
	}
	if err != nil {
		return nil, execenv.Error("dial", execenv.ErrConnection)
	}
	client := &Client{
		cfg:     cfg,
		sess:    newSession(conn),
		pending: make(map[uint64]chan frame),
		terms:   make(map[execenv.ID]*remoteTerminal),
		obs:     make(map[execenv.ID]*remoteObservation),
	}
	go client.readLoop()
	if err := client.authenticate(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// Close ends the session. Live grants on the host are not revoked.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.sess.Close()
}

func (c *Client) authenticate() error {
	extra, err := encodeExtra(authArgs{Token: c.cfg.Token, Release: execenv.Release})
	if err != nil {
		return execenv.Error("auth", err)
	}
	_, err = c.call(context.Background(), methodAuth, "", extra)
	return err
}

func (c *Client) readLoop() {
	for {
		f, err := c.sess.Recv()
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
		ch <- frame{Seq: seq, Status: "connection"}
		delete(c.pending, seq)
	}
	for id, term := range c.terms {
		term.hangup(execenv.ErrClosed)
		delete(c.terms, id)
	}
	for id, obs := range c.obs {
		obs.fail(execenv.ErrConnection)
		delete(c.obs, id)
	}
}

func (c *Client) deliverPty(f frame) {
	c.mu.Lock()
	term := c.terms[execenv.ID(f.Grant)]
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
	obs := c.obs[execenv.ID(f.Grant)]
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

func (c *Client) call(ctx context.Context, method, grant string, extra []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error(method, err)
	}
	seq := c.seq.Add(1)
	ch := make(chan frame, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, execenv.Error(method, execenv.ErrConnection)
	}
	c.pending[seq] = ch
	c.mu.Unlock()
	err := c.sess.Send(frame{
		Seq:    seq,
		Kind:   kindRequest,
		Method: method,
		Grant:  grant,
		Extra:  extra,
	})
	if err != nil {
		return nil, execenv.Error(method, execenv.ErrConnection)
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

// Capabilities is local. A remote host is isolated; the wire does not name
// how isolation is implemented.
func (c *Client) Capabilities() execenv.Capabilities {
	return execenv.Capabilities{Isolated: true, Freeze: true}
}

func (c *Client) Ready(ctx context.Context) (execenv.Report, error) {
	extra, err := c.call(ctx, methodReady, "", nil)
	if err != nil {
		return execenv.Report{}, err
	}
	var report execenv.Report
	if err := decodeExtra(extra, &report); err != nil {
		return execenv.Report{}, execenv.Error("ready", execenv.ErrUnavailable)
	}
	return report, nil
}

func (c *Client) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	extra, err := encodeExtra(ensureArgs{Spec: spec})
	if err != nil {
		return nil, execenv.Error("ensure", err)
	}
	if _, err := c.call(ctx, methodEnsure, string(spec.ID), extra); err != nil {
		return nil, err
	}
	return &remoteEnv{client: c, id: spec.ID}, nil
}

func (c *Client) Revoke(ctx context.Context, id execenv.ID) error {
	_, err := c.call(ctx, methodRevoke, string(id), nil)
	return err
}

type remoteEnv struct {
	client *Client
	id     execenv.ID
}

func (e *remoteEnv) ID() execenv.ID { return e.id }

func (e *remoteEnv) Attach(ctx context.Context, win execenv.Window) (execenv.Terminal, error) {
	extra, err := encodeExtra(attachArgs{Window: win})
	if err != nil {
		return nil, execenv.Error("attach", err)
	}
	if _, err := e.client.call(ctx, methodAttach, string(e.id), extra); err != nil {
		return nil, err
	}
	term := newRemoteTerminal(e.client, e.id)
	e.client.mu.Lock()
	e.client.terms[e.id] = term
	e.client.mu.Unlock()
	return term, nil
}

func (e *remoteEnv) ReplaceTree(ctx context.Context, tree execenv.Tree) error {
	extra, err := encodeExtra(replaceArgs{Tree: tree})
	if err != nil {
		return execenv.Error("replace", err)
	}
	_, err = e.client.call(ctx, methodReplace, string(e.id), extra)
	return err
}

func (e *remoteEnv) Apply(ctx context.Context, batch execenv.Batch) error {
	extra, err := encodeExtra(applyArgs{Batch: batch})
	if err != nil {
		return execenv.Error("apply", err)
	}
	_, err = e.client.call(ctx, methodApply, string(e.id), extra)
	return err
}

func (e *remoteEnv) Watch(ctx context.Context) (execenv.Observation, error) {
	if _, err := e.client.call(ctx, methodWatch, string(e.id), nil); err != nil {
		return nil, err
	}
	obs := newRemoteObservation(e.client, e.id)
	e.client.mu.Lock()
	e.client.obs[e.id] = obs
	e.client.mu.Unlock()
	return obs, nil
}

func (e *remoteEnv) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	extra, err := encodeExtra(openArgs{Path: path})
	if err != nil {
		return nil, execenv.Error("open", err)
	}
	out, err := e.client.call(ctx, methodOpen, string(e.id), extra)
	if err != nil {
		return nil, err
	}
	var body []byte
	if err := decodeExtra(out, &body); err != nil {
		return nil, execenv.Error("open", execenv.ErrUnavailable)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (e *remoteEnv) Freeze(ctx context.Context) error {
	_, err := e.client.call(ctx, methodFreeze, string(e.id), nil)
	e.client.hangupTerm(e.id, execenv.ErrFrozen)
	return err
}

func (e *remoteEnv) Thaw(ctx context.Context) error {
	_, err := e.client.call(ctx, methodThaw, string(e.id), nil)
	return err
}

func (e *remoteEnv) Revoke(ctx context.Context) error {
	err := e.client.Revoke(ctx, e.id)
	e.client.hangupTerm(e.id, execenv.ErrRevoked)
	return err
}

func (c *Client) hangupTerm(id execenv.ID, err error) {
	c.mu.Lock()
	term := c.terms[id]
	delete(c.terms, id)
	c.mu.Unlock()
	if term != nil {
		term.hangup(err)
	}
}

type remoteTerminal struct {
	client *Client
	id     execenv.ID
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	dead   error
}

func newRemoteTerminal(client *Client, id execenv.ID) *remoteTerminal {
	t := &remoteTerminal{client: client, id: id}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *remoteTerminal) push(p []byte) {
	t.mu.Lock()
	if !t.closed && t.dead == nil {
		t.buf = append(t.buf, p...)
		t.cond.Broadcast()
	}
	t.mu.Unlock()
}

func (t *remoteTerminal) hangup(err error) {
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

func (t *remoteTerminal) Read(p []byte) (int, error) {
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

func (t *remoteTerminal) Write(p []byte) (int, error) {
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
	// PTY octets travel as kindPty, not as an RPC, so a hangup does not
	// take the control channel down with it.
	err := t.client.sess.Send(frame{
		Kind:  kindPty,
		Grant: string(t.id),
		Extra: append([]byte(nil), p...),
	})
	if err != nil {
		return 0, execenv.Error("write", execenv.ErrClosed)
	}
	return len(p), nil
}

func (t *remoteTerminal) Resize(ctx context.Context, win execenv.Window) error {
	extra, err := encodeExtra(resizeArgs{Window: win})
	if err != nil {
		return execenv.Error("resize", err)
	}
	_, err = t.client.call(ctx, methodResize, string(t.id), extra)
	return err
}

func (t *remoteTerminal) Close() error {
	t.mu.Lock()
	t.closed = true
	t.cond.Broadcast()
	t.mu.Unlock()
	_, err := t.client.call(context.Background(), methodDetach, string(t.id), nil)
	t.client.mu.Lock()
	delete(t.client.terms, t.id)
	t.client.mu.Unlock()
	return err
}

type remoteObservation struct {
	client *Client
	id     execenv.ID
	events chan execenv.Event
	errc   chan error
	once   sync.Once
}

func newRemoteObservation(client *Client, id execenv.ID) *remoteObservation {
	return &remoteObservation{
		client: client,
		id:     id,
		events: make(chan execenv.Event, watchBufferRemote),
		errc:   make(chan error, 1),
	}
}

const watchBufferRemote = 32

func (o *remoteObservation) push(ev execenv.Event) {
	select {
	case o.events <- ev:
	default:
		o.fail(execenv.ErrLagged)
	}
}

func (o *remoteObservation) fail(err error) {
	o.once.Do(func() {
		o.errc <- err
		close(o.events)
	})
}

func (o *remoteObservation) Next(ctx context.Context) (execenv.Event, error) {
	select {
	case <-ctx.Done():
		return execenv.Event{}, execenv.Error("watch", ctx.Err())
	case ev, ok := <-o.events:
		if ok {
			return ev, nil
		}
	}
	// events is closed after fail. Prefer the recorded cause (lag, revoke,
	// connection) over a generic closed so the caller knows to resync.
	select {
	case err := <-o.errc:
		return execenv.Event{}, execenv.Error("watch", err)
	default:
		return execenv.Event{}, execenv.Error("watch", execenv.ErrClosed)
	}
}

func (o *remoteObservation) Close() error {
	o.fail(execenv.ErrClosed)
	_, err := o.client.call(context.Background(), methodUnwatch, string(o.id), nil)
	o.client.mu.Lock()
	delete(o.client.obs, o.id)
	o.client.mu.Unlock()
	return err
}
