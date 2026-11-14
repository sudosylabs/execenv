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
	// terms is keyed by stream. Several streams may name one grant while a
	// takeover is settling; only the host decides which one is attached.
	terms  map[uint64]*remoteTerminal
	closed bool
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
		terms:   make(map[uint64]*remoteTerminal),
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
		case kindCredit:
			c.deliverCredit(f)
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
	// A lost connection is a detach on the host. Each Terminal keeps its
	// Offset so a new connection can resume it.
	for stream, term := range c.terms {
		term.hangup(execenv.ErrConnection)
		delete(c.terms, stream)
	}
}

func (c *Client) termFor(f frame) *remoteTerminal {
	c.mu.Lock()
	term := c.terms[f.Stream]
	c.mu.Unlock()
	if term == nil || string(term.id) != f.Grant {
		return nil
	}
	return term
}

// deliverPty never blocks the read loop: credit bounds what one terminal
// buffers, so a caller that stops reading delays nothing else.
func (c *Client) deliverPty(f frame) {
	term := c.termFor(f)
	if term == nil {
		return
	}
	switch f.Status {
	case "":
		if !term.push(f.Seq, f.Extra) {
			c.failStream(term)
		}
	case statusEOF:
		c.forget(term)
		term.finish(io.EOF)
	default:
		c.forget(term)
		term.hangup(errorFromStatus(f.Status))
	}
}

func (c *Client) deliverCredit(f frame) {
	term := c.termFor(f)
	if term != nil && !term.grantInput(f.Seq) {
		c.failStream(term)
	}
}

// failStream ends one terminal whose host broke flow control and detaches
// it without blocking the read loop. The connection and other terminals
// are unaffected.
func (c *Client) failStream(term *remoteTerminal) {
	c.forget(term)
	term.hangup(execenv.ErrInvalid)
	go func() { _ = term.detach() }()
}

func (c *Client) forget(term *remoteTerminal) {
	c.mu.Lock()
	if c.terms[term.stream] == term {
		delete(c.terms, term.stream)
	}
	c.mu.Unlock()
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

func (e *remoteEnv) Attach(ctx context.Context, request execenv.AttachRequest) (execenv.Terminal, error) {
	if e.revoked.Load() {
		return nil, execenv.ErrRevoked
	}
	stream := e.client.stream.Add(1)
	extra, err := encodeExtra(attachArgs{Window: request.Window, Stream: stream, Resume: request.Resume, Takeover: request.Takeover, Credit: ptyWindow})
	if err != nil {
		return nil, execenv.Error("attach", err)
	}
	term := newRemoteTerminal(e.client, e.id, stream)
	term.epoch = e.epoch
	// Register before the call: output may arrive before the reply.
	e.client.mu.Lock()
	e.client.terms[stream] = term
	e.client.mu.Unlock()
	out, err := e.call(ctx, methodAttach, string(e.id), extra)
	var result attachResult
	if err == nil {
		if decodeErr := decodeExtra(out, &result); decodeErr != nil || result.Terminal == "" || result.Credit == 0 || result.Credit > maxPtyWindow {
			err = execenv.Error("attach", execenv.ErrUnavailable)
		}
	}
	if err != nil {
		e.client.forget(term)
		term.hangup(err)
		return nil, err
	}
	term.setStart(result.Terminal, result.Start, result.Credit)
	// The host attached this stream; any earlier stream of this client for
	// the grant is replaced, whatever frames are still in flight for it.
	e.client.mu.Lock()
	var replaced []*remoteTerminal
	for other, previous := range e.client.terms {
		if other != stream && previous.id == e.id {
			replaced = append(replaced, previous)
			delete(e.client.terms, other)
		}
	}
	e.client.mu.Unlock()
	for _, previous := range replaced {
		previous.hangup(execenv.ErrSuperseded)
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

func (e *remoteEnv) terminals() []*remoteTerminal {
	e.client.mu.Lock()
	defer e.client.mu.Unlock()
	var out []*remoteTerminal
	for _, term := range e.client.terms {
		if term.id == e.id && term.epoch == e.epoch {
			out = append(out, term)
		}
	}
	return out
}

func (e *remoteEnv) setTermFrozen(frozen bool) {
	for _, term := range e.terminals() {
		term.mu.Lock()
		term.frozen = frozen
		term.cond.Broadcast()
		term.mu.Unlock()
	}
}

// remoteTerminal is one attached stream. It buffers at most ptyWindow
// bytes of output as segments that remember their absolute terminal
// offsets: the host sends no more than the credit this side granted, and
// Read returns credit as the caller consumes. Write sends input only
// within the host's credit.
type remoteTerminal struct {
	epoch  string
	client *Client
	id     execenv.ID
	stream uint64
	// writeMu keeps one Write's frames contiguous.
	writeMu sync.Mutex

	mu       sync.Mutex
	cond     *sync.Cond
	terminal execenv.TerminalID
	start    uint64
	offset   uint64
	segments []ptySegment
	buffered int
	// outCredit is output the host may still send; outUsed is output read
	// and not yet granted back.
	outCredit uint64
	outUsed   uint64
	// inCredit is input the host still accepts, at most inWindow.
	inWindow uint64
	inCredit uint64
	closed   bool
	frozen   bool
	// ended is the shell's exit (io.EOF), delivered after the buffer drains.
	ended error
	// dead fails the terminal; buffered output still drains first.
	dead error
}

type ptySegment struct {
	offset uint64
	data   []byte
}

var _ execenv.Terminal = (*remoteTerminal)(nil)

func newRemoteTerminal(client *Client, id execenv.ID, stream uint64) *remoteTerminal {
	t := &remoteTerminal{client: client, id: id, stream: stream, outCredit: ptyWindow}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *remoteTerminal) setStart(id execenv.TerminalID, start, inputWindow uint64) {
	t.mu.Lock()
	t.terminal = id
	t.start = start
	t.inWindow = inputWindow
	t.inCredit = inputWindow
	if len(t.segments) > 0 {
		t.offset = t.segments[0].offset
	} else {
		t.offset = start
	}
	t.cond.Broadcast()
	t.mu.Unlock()
}

// push buffers one frame. It reports false when the frame exceeds the
// credit this side granted: the host broke flow control.
func (t *remoteTerminal) push(offset uint64, p []byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if uint64(len(p)) > t.outCredit {
		return false
	}
	t.outCredit -= uint64(len(p))
	if t.closed || t.dead != nil || t.ended != nil {
		return true
	}
	t.segments = append(t.segments, ptySegment{offset: offset, data: append([]byte(nil), p...)})
	t.buffered += len(p)
	t.cond.Broadcast()
	return true
}

// grantInput adds host credit. It reports false when the grant would
// exceed the window: the host returned bytes it was never sent.
func (t *remoteTerminal) grantInput(n uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n > t.inWindow-t.inCredit {
		return false
	}
	t.inCredit += n
	t.cond.Broadcast()
	return true
}

func (t *remoteTerminal) finish(err error) {
	t.mu.Lock()
	if t.ended == nil && t.dead == nil {
		t.ended = err
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

func (t *remoteTerminal) ID() execenv.TerminalID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.terminal
}

func (t *remoteTerminal) Start() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.start
}

func (t *remoteTerminal) Offset() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.offset
}

// Read drains buffered output before reporting why the terminal ended.
// Superseded and closed terminals stop at once. Once at least half the
// window has been read, Read returns that credit to the host.
func (t *remoteTerminal) Read(p []byte) (int, error) {
	n, grant, err := t.read(p)
	if grant > 0 {
		// A failed send means the connection is gone; the read loop
		// reports that.
		_ = t.client.sess.Send(frame{Seq: grant, Kind: kindCredit, Grant: string(t.id), Stream: t.stream})
	}
	return n, err
}

func (t *remoteTerminal) read(p []byte) (int, uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.segments) == 0 && !t.closed && t.dead == nil && t.ended == nil {
		t.cond.Wait()
	}
	if t.closed {
		return 0, 0, execenv.Error("read", execenv.ErrClosed)
	}
	if errors.Is(t.dead, execenv.ErrSuperseded) || errors.Is(t.dead, execenv.ErrRevoked) || errors.Is(t.dead, execenv.ErrLost) {
		return 0, 0, execenv.Error("read", t.dead)
	}
	if len(t.segments) > 0 {
		seg := &t.segments[0]
		// A segment past the cursor is output the host skipped because this
		// reader fell behind its history.
		t.offset = max(t.offset, seg.offset)
		n := copy(p, seg.data)
		seg.data = seg.data[n:]
		seg.offset += uint64(n)
		t.offset += uint64(n)
		t.buffered -= n
		if len(seg.data) == 0 {
			t.segments = t.segments[1:]
		}
		var grant uint64
		t.outUsed += uint64(n)
		if t.outUsed >= ptyWindow/2 && t.dead == nil && t.ended == nil {
			grant = t.outUsed
			t.outUsed = 0
			t.outCredit += grant
		}
		return n, grant, nil
	}
	if t.dead != nil {
		return 0, 0, execenv.Error("read", t.dead)
	}
	return 0, 0, t.ended
}

func (t *remoteTerminal) liveLocked() error {
	if t.closed {
		return execenv.ErrClosed
	}
	if t.dead != nil {
		return t.dead
	}
	if t.ended != nil {
		return execenv.ErrClosed
	}
	if t.frozen {
		return execenv.ErrFrozen
	}
	return nil
}

// Write sends p within the host's input credit, in frames of at most
// mux.MaxPtyBytes. It blocks while the host has granted none and returns
// once the terminal is closed, superseded, revoked, frozen or its
// connection lost, reporting how much of p it sent.
func (t *remoteTerminal) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	written := 0
	for {
		t.mu.Lock()
		for {
			if err := t.liveLocked(); err != nil {
				t.mu.Unlock()
				return written, execenv.Error("write", err)
			}
			if len(p) == 0 || t.inCredit > 0 {
				break
			}
			t.cond.Wait()
		}
		if len(p) == 0 {
			t.mu.Unlock()
			return written, nil
		}
		n := int(min(uint64(len(p)), t.inCredit, mux.MaxPtyBytes))
		t.inCredit -= uint64(n)
		t.mu.Unlock()
		// PTY octets travel as kindPty, not as an RPC, so a hangup does
		// not take the control channel down with it.
		err := t.client.sess.Send(frame{
			Kind:   kindPty,
			Grant:  string(t.id),
			Stream: t.stream,
			Extra:  append([]byte(nil), p[:n]...),
		})
		if err != nil {
			return written, execenv.Error("write", execenv.ErrConnection)
		}
		written += n
		p = p[n:]
	}
}

func (t *remoteTerminal) Resize(ctx context.Context, win execenv.Window) error {
	t.mu.Lock()
	err := t.liveLocked()
	t.mu.Unlock()
	if err != nil {
		return execenv.Error("resize", err)
	}
	extra, err := encodeExtra(resizeArgs{Window: win, Stream: t.stream})
	if err != nil {
		return execenv.Error("resize", err)
	}
	_, err = t.client.call(ctx, methodResize, string(t.id), extra)
	return err
}

// Close detaches. The shell keeps running on the host for its grace.
func (t *remoteTerminal) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.cond.Broadcast()
	t.mu.Unlock()
	return t.detach()
}

func (t *remoteTerminal) detach() error {
	t.client.forget(t)
	extra, err := encodeExtra(streamArgs{Stream: t.stream})
	if err != nil {
		return execenv.Error("detach", err)
	}
	_, err = t.client.call(context.Background(), methodDetach, string(t.id), extra)
	if errors.Is(err, execenv.ErrConnection) {
		// The host detaches a lost connection's terminals itself.
		return nil
	}
	return err
}

// Hangup ends the shell on the host now.
func (t *remoteTerminal) Hangup(ctx context.Context) error {
	t.mu.Lock()
	err := t.liveLocked()
	t.mu.Unlock()
	if err != nil && !errors.Is(err, execenv.ErrFrozen) {
		return execenv.Error("hangup", err)
	}
	extra, err := encodeExtra(streamArgs{Stream: t.stream})
	if err != nil {
		return execenv.Error("hangup", err)
	}
	if _, err := t.client.call(ctx, methodHangup, string(t.id), extra); err != nil {
		return err
	}
	t.client.forget(t)
	t.hangup(execenv.ErrClosed)
	return nil
}
