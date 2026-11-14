package remote

import (
	"context"
	"crypto/tls"
	"errors"
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
	capabilities execenv.Capabilities
	cfg          Config
	sess         *mux.Session
	seq          atomic.Uint64
	stream       atomic.Uint64
	mu           sync.Mutex
	pending      map[uint64]chan frame
	terms        map[execenv.ID]*remoteTerminal
	closed       bool
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
		if tlsCfg.MinVersion == 0 {
			tlsCfg.MinVersion = tls.VersionTLS13
		}
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
	extra, err := encodeExtra(authArgs{Token: c.cfg.Token, Release: execenv.Release, Protocol: protocolVersion})
	if err != nil {
		return execenv.Error("auth", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeoutOrDefault(c.cfg.Timeout))
	defer cancel()
	out, err := c.call(ctx, methodAuth, "", extra)
	if err != nil {
		return err
	}
	var result authResult
	if err := decodeExtra(out, &result); err != nil || result.Protocol != protocolVersion {
		return execenv.ErrUnavailable
	}
	c.capabilities = result.Capabilities
	return nil
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
}

func (c *Client) deliverPty(f frame) {
	c.mu.Lock()
	term := c.terms[execenv.ID(f.Grant)]
	c.mu.Unlock()
	if term == nil || term.stream != f.Stream {
		return
	}
	if f.Status != "" {
		term.hangup(errorFromStatus(f.Status))
		return
	}
	term.push(f.Extra)
}

func (c *Client) call(ctx context.Context, method, grant string, extra []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error(method, err)
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeoutOrDefault(c.cfg.OperationTimeout))
	defer cancel()
	seq := c.seq.Add(1)
	ch := make(chan frame, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, execenv.Error(method, execenv.ErrConnection)
	}
	c.pending[seq] = ch
	c.mu.Unlock()
	deadline, _ := ctx.Deadline()
	err := c.sess.Send(frame{
		Seq:      seq,
		Kind:     kindRequest,
		Method:   method,
		Grant:    grant,
		Extra:    extra,
		Deadline: deadline.UnixNano(),
	})
	if err != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		if errors.Is(err, execenv.ErrTooLarge) || errors.Is(err, execenv.ErrInvalid) {
			return nil, execenv.Error(method, err)
		}
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

// Capabilities is what the served host reported at authentication. The wire
// does not name how isolation is implemented.
func (c *Client) Capabilities() execenv.Capabilities {
	return c.capabilities
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
	out, err := c.call(ctx, methodEnsure, string(spec.ID), extra)
	if err != nil {
		return nil, err
	}
	var result ensureResult
	if err := decodeExtra(out, &result); err != nil {
		return nil, execenv.ErrUnavailable
	}
	return &remoteEnv{client: c, id: spec.ID, epoch: result.Epoch}, nil
}

func (c *Client) Revoke(ctx context.Context, id execenv.ID) error {
	_, err := c.call(ctx, methodRevoke, string(id), nil)
	return err
}

type remoteEnv struct {
	revoked atomic.Bool
	epoch   string
	client  *Client
	id      execenv.ID
}

func (e *remoteEnv) ID() execenv.ID { return e.id }

func (e *remoteEnv) Attach(ctx context.Context, win execenv.Window) (execenv.Terminal, error) {
	if e.revoked.Load() {
		return nil, execenv.ErrRevoked
	}
	stream := e.client.stream.Add(1)
	extra, err := encodeExtra(attachArgs{Window: win, Stream: stream})
	if err != nil {
		return nil, execenv.Error("attach", err)
	}
	term := newRemoteTerminal(e.client, e.id, stream)
	term.epoch = e.epoch
	e.client.mu.Lock()
	if current := e.client.terms[e.id]; current != nil {
		current.mu.Lock()
		frozen := current.frozen
		current.mu.Unlock()
		e.client.mu.Unlock()
		if frozen {
			return nil, execenv.ErrFrozen
		}
		return nil, execenv.Error("attach", execenv.ErrBusy)
	}
	e.client.terms[e.id] = term
	e.client.mu.Unlock()
	if _, err := e.call(ctx, methodAttach, string(e.id), extra); err != nil {
		e.client.mu.Lock()
		if e.client.terms[e.id] == term {
			delete(e.client.terms, e.id)
		}
		e.client.mu.Unlock()
		term.hangup(err)
		return nil, err
	}
	return term, nil
}

func (e *remoteEnv) Revoke(ctx context.Context) error {
	extra, err := encodeExtra(e.epoch)
	if err != nil {
		return err
	}
	_, err = e.client.call(ctx, methodRevokeEpoch, string(e.id), extra)
	if err == nil {
		e.revoked.Store(true)
		e.hangupTerm(execenv.ErrRevoked)
	}
	return err
}

func (e *remoteEnv) setTermFrozen(frozen bool) {
	e.client.mu.Lock()
	term := e.client.terms[e.id]
	e.client.mu.Unlock()
	if term != nil && term.epoch == e.epoch {
		term.mu.Lock()
		term.frozen = frozen
		term.cond.Broadcast()
		term.mu.Unlock()
	}
}

type remoteTerminal struct {
	epoch  string
	client *Client
	id     execenv.ID
	stream uint64
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	frozen bool
	dead   error
}

func newRemoteTerminal(client *Client, id execenv.ID, stream uint64) *remoteTerminal {
	t := &remoteTerminal{client: client, id: id, stream: stream}
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
	if t.frozen {
		t.mu.Unlock()
		return 0, execenv.ErrFrozen
	}
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
		Kind:   kindPty,
		Grant:  string(t.id),
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

func (t *remoteTerminal) Resize(ctx context.Context, win execenv.Window) error {
	t.mu.Lock()
	frozen := t.frozen
	t.mu.Unlock()
	if frozen {
		return execenv.ErrFrozen
	}
	extra, err := encodeExtra(resizeArgs{Window: win, Stream: t.stream})
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
	extra, encodeErr := encodeExtra(streamArgs{Stream: t.stream})
	if encodeErr != nil {
		return execenv.Error("detach", encodeErr)
	}
	_, err := t.client.call(context.Background(), methodDetach, string(t.id), extra)
	t.client.mu.Lock()
	if t.client.terms[t.id] == t {
		delete(t.client.terms, t.id)
	}
	t.client.mu.Unlock()
	return err
}
