package remote

import (
	"context"
	"errors"
	"io"
	"sync"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

// Attach opens the terminal's output stream and waits for the host to
// attach it. The stream outlives ctx: it ends with Close, Hangup, the
// shell's exit, or the client's Close.
func (e *remoteEnv) Attach(ctx context.Context, request execenv.AttachRequest) (execenv.Terminal, error) {
	if err := e.alive(); err != nil {
		return nil, err
	}
	msg := &execenvv1.AttachRequest{GrantId: string(e.id), Epoch: e.epoch, Window: windowTo(request.Window), Takeover: request.Takeover}
	if request.Resume != nil {
		msg.Resume = &execenvv1.TerminalPosition{TerminalId: string(request.Resume.Terminal), Offset: request.Resume.Offset}
	}
	sctx, cancel := context.WithCancel(e.client.ctx)
	stream, err := e.client.terminal.Attach(sctx, connect.NewRequest(msg))
	if err != nil {
		cancel()
		return nil, hostError("attach", err)
	}
	attached := make(chan error, 1)
	var first *execenvv1.Attached
	go func() {
		if !stream.Receive() {
			attached <- streamEnd(stream.Err())
			return
		}
		first = stream.Msg().GetAttached()
		if first == nil || first.GetAttachmentId() == "" || first.GetTerminalId() == "" {
			attached <- execenv.ErrUnavailable
			return
		}
		attached <- nil
	}()
	select {
	case err = <-attached:
	case <-ctx.Done():
		cancel()
		<-attached
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		return nil, execenv.Error("attach", err)
	}
	t := &remoteTerminal{
		env:      e,
		stream:   stream,
		ctx:      sctx,
		cancel:   cancel,
		key:      first.GetAttachmentId(),
		terminal: execenv.TerminalID(first.GetTerminalId()),
		start:    first.GetStart(),
		offset:   first.GetStart(),
	}
	e.mu.Lock()
	if e.terms == nil {
		e.terms = make(map[*remoteTerminal]struct{})
	}
	e.terms[t] = struct{}{}
	e.mu.Unlock()
	if e.revoked.Load() {
		t.end(execenv.ErrRevoked)
	}
	return t, nil
}

// streamEnd is why an output stream ended: nil error is the shell's exit.
func streamEnd(err error) error {
	if err == nil {
		return io.EOF
	}
	return hostError("read", err)
}

// streamEndFor is streamEnd, except that a stream ended by the client's own
// Close is a lost connection: the host detaches and the shell stays.
func (t *remoteTerminal) streamEndFor(err error) error {
	if t.env.client.ctx.Err() != nil {
		return execenv.Error("read", execenv.ErrConnection)
	}
	return streamEnd(err)
}

func (e *remoteEnv) forget(t *remoteTerminal) {
	e.mu.Lock()
	delete(e.terms, t)
	e.mu.Unlock()
}

func (e *remoteEnv) terminals() []*remoteTerminal {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*remoteTerminal, 0, len(e.terms))
	for t := range e.terms {
		out = append(out, t)
	}
	return out
}

func (e *remoteEnv) setFrozen(frozen bool) {
	for _, t := range e.terminals() {
		t.mu.Lock()
		t.frozen = frozen
		t.mu.Unlock()
	}
}

func (e *remoteEnv) endTerminals(err error) {
	for _, t := range e.terminals() {
		t.end(err)
	}
}

// remoteTerminal is one attachment. Output is pulled from the stream as the
// caller reads, so the client holds at most one message beyond what HTTP/2
// flow control allows in flight.
type remoteTerminal struct {
	env      *remoteEnv
	stream   *connect.ServerStreamForClient[execenvv1.AttachResponse]
	ctx      context.Context
	cancel   context.CancelFunc
	key      string
	terminal execenv.TerminalID
	start    uint64

	// readMu serializes Read; mu guards the state below.
	readMu  sync.Mutex
	writeMu sync.Mutex
	input   *connect.ClientStreamForClient[execenvv1.InputRequest, execenvv1.InputResponse]

	mu      sync.Mutex
	offset  uint64
	pending []byte
	closed  bool
	frozen  bool
	// dead ends the terminal for this caller; ended is how the stream ended.
	dead  error
	ended error
}

var _ execenv.Terminal = (*remoteTerminal)(nil)

func (t *remoteTerminal) ID() execenv.TerminalID { return t.terminal }
func (t *remoteTerminal) Start() uint64          { return t.start }

func (t *remoteTerminal) Offset() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.offset
}

// end fails the terminal for err and releases its stream.
func (t *remoteTerminal) end(err error) {
	t.mu.Lock()
	if t.dead == nil {
		t.dead = err
	}
	t.mu.Unlock()
	t.cancel()
	t.env.forget(t)
}

// Read drains what has arrived before reporting why the terminal ended.
// A closed, superseded, revoked or lost terminal stops at once.
func (t *remoteTerminal) Read(p []byte) (int, error) {
	t.readMu.Lock()
	defer t.readMu.Unlock()
	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return 0, execenv.Error("read", execenv.ErrClosed)
		}
		if t.dead != nil {
			err := t.dead
			t.mu.Unlock()
			return 0, execenv.Error("read", err)
		}
		if len(t.pending) > 0 {
			n := copy(p, t.pending)
			t.pending = t.pending[n:]
			t.offset += uint64(n)
			t.mu.Unlock()
			return n, nil
		}
		if t.ended != nil {
			err := t.ended
			t.mu.Unlock()
			return 0, err
		}
		t.mu.Unlock()

		if !t.stream.Receive() {
			err := t.streamEndFor(t.stream.Err())
			t.mu.Lock()
			if errors.Is(err, execenv.ErrSuperseded) || errors.Is(err, execenv.ErrRevoked) || errors.Is(err, execenv.ErrLost) {
				if t.dead == nil {
					t.dead = err
				}
			} else if t.ended == nil {
				t.ended = err
			}
			t.mu.Unlock()
			t.env.forget(t)
			continue
		}
		out := t.stream.Msg().GetOutput()
		if out == nil {
			continue
		}
		t.mu.Lock()
		// Output past the cursor is history this reader fell behind.
		t.offset = max(t.offset, out.GetOffset())
		t.pending = out.GetData()
		t.mu.Unlock()
	}
}

func (t *remoteTerminal) liveLocked() error {
	switch {
	case t.closed:
		return execenv.ErrClosed
	case t.dead != nil:
		return t.dead
	case t.ended != nil:
		return execenv.ErrClosed
	case t.frozen:
		return execenv.ErrFrozen
	}
	return nil
}

// Write sends p in order on the attachment's input stream. HTTP/2 flow
// control holds it back while the host is not reading.
func (t *remoteTerminal) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	err := t.liveLocked()
	t.mu.Unlock()
	if err != nil {
		return 0, execenv.Error("write", err)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if t.input == nil {
		input := t.env.client.terminal.Input(t.streamContext())
		if err := input.Send(&execenvv1.InputRequest{Part: &execenvv1.InputRequest_AttachmentId{AttachmentId: t.key}}); err != nil {
			return 0, t.inputFailed(input)
		}
		t.input = input
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), chunkBytes)
		if err := t.input.Send(&execenvv1.InputRequest{Part: &execenvv1.InputRequest_Data{Data: p[:n]}}); err != nil {
			input := t.input
			t.input = nil
			return written, t.inputFailed(input)
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// streamContext lives as long as the attachment.
func (t *remoteTerminal) streamContext() context.Context { return t.ctx }

func (t *remoteTerminal) inputFailed(input *connect.ClientStreamForClient[execenvv1.InputRequest, execenvv1.InputResponse]) error {
	_, err := input.CloseAndReceive()
	if err == nil {
		err = execenv.ErrClosed
	}
	return hostError("write", err)
}

// drainInput closes the input stream and waits until the host has written
// every byte sent, so a caller's last keystrokes survive its own Close.
func (t *remoteTerminal) drainInput() {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.input != nil {
		_, _ = t.input.CloseAndReceive()
		t.input = nil
	}
}

func (t *remoteTerminal) Resize(ctx context.Context, win execenv.Window) error {
	t.mu.Lock()
	err := t.liveLocked()
	t.mu.Unlock()
	if err != nil {
		return execenv.Error("resize", err)
	}
	ctx, cancel := t.env.client.call(ctx)
	defer cancel()
	_, err = t.env.client.terminal.Resize(ctx, connect.NewRequest(&execenvv1.ResizeRequest{AttachmentId: t.key, Window: windowTo(win)}))
	return hostError("resize", err)
}

// Close detaches. The shell keeps running on the host for its grace.
func (t *remoteTerminal) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.drainInput()
	// A lost connection fails Detach; the host detaches that stream itself.
	ctx, cancel := t.env.client.call(context.Background())
	_, _ = t.env.client.terminal.Detach(ctx, connect.NewRequest(&execenvv1.DetachRequest{AttachmentId: t.key}))
	cancel()
	t.cancel()
	t.env.forget(t)
	return nil
}

// Hangup ends the shell on the host now.
func (t *remoteTerminal) Hangup(ctx context.Context) error {
	t.mu.Lock()
	err := t.liveLocked()
	t.mu.Unlock()
	if err != nil && !errors.Is(err, execenv.ErrFrozen) {
		return execenv.Error("hangup", err)
	}
	t.drainInput()
	cctx, cancel := t.env.client.call(ctx)
	defer cancel()
	if _, err := t.env.client.terminal.Hangup(cctx, connect.NewRequest(&execenvv1.HangupRequest{AttachmentId: t.key})); err != nil {
		return hostError("hangup", err)
	}
	t.end(execenv.ErrClosed)
	return nil
}
