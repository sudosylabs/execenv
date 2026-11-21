package wire

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/portconn"
)

// Credit includes both buffered data and bytes the sender can still send.
const portWindow = 256 << 10

// MaxPortMessage leaves room for the frame's protobuf overhead.
const MaxPortMessage = portconn.ChunkSize + 256

type portStream interface {
	Send(*execenvv1.PortFrame) error
	Receive() (*execenvv1.PortFrame, error)
}

type portClient = connect.BidiStreamForClient[execenvv1.PortFrame, execenvv1.PortFrame]

type portFlow struct {
	ctx     context.Context
	stream  portStream
	sendMu  sync.Mutex
	mu      sync.Mutex
	change  chan struct{}
	credit  int // Credit received from the peer.
	granted int // Credit the peer has yet to spend.
	in      []byte
	fin     bool
}

func newPortFlow(ctx context.Context, stream portStream) *portFlow {
	return &portFlow{ctx: ctx, stream: stream, change: make(chan struct{})}
}

func (f *portFlow) notifyLocked() {
	close(f.change)
	f.change = make(chan struct{})
}

func (f *portFlow) send(frame *execenvv1.PortFrame) error {
	f.sendMu.Lock()
	defer f.sendMu.Unlock()
	if err := context.Cause(f.ctx); err != nil {
		return err
	}
	return f.stream.Send(frame)
}

func (f *portFlow) receive() error {
	for {
		frame, err := f.stream.Receive()
		if err != nil {
			f.mu.Lock()
			fin := f.fin
			f.mu.Unlock()
			if errors.Is(err, io.EOF) && fin {
				return nil
			}
			return err
		}
		if err := f.accept(frame); err != nil {
			return err
		}
	}
}

func (f *portFlow) accept(frame *execenvv1.PortFrame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch part := frame.GetPayload().(type) {
	case *execenvv1.PortFrame_Credit:
		if part.Credit == 0 || part.Credit > uint32(portWindow-f.credit) {
			return execenv.ErrInvalid
		}
		f.credit += int(part.Credit)
	case *execenvv1.PortFrame_Data:
		n := len(part.Data)
		if f.fin || n == 0 || n > portconn.ChunkSize || n > f.granted {
			return execenv.ErrInvalid
		}
		f.granted -= n
		f.in = append(f.in, part.Data...)
	case *execenvv1.PortFrame_CloseWrite:
		if f.fin || !part.CloseWrite {
			return execenv.ErrInvalid
		}
		f.fin = true
	default:
		return execenv.ErrInvalid
	}
	f.notifyLocked()
	return nil
}

func (f *portFlow) read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		f.mu.Lock()
		outstanding := f.granted + len(f.in)
		amount := 0
		if outstanding < portWindow/2 && !f.fin && f.ctx.Err() == nil {
			// Only the reader replenishes credit. A throttled relay
			// cannot call read again until it has paid for the last read.
			amount = portWindow - outstanding
			f.granted += amount
		}
		var n int
		if len(f.in) > 0 {
			n = copy(p, f.in)
			f.in = f.in[n:]
			if len(f.in) == 0 {
				f.in = nil
			}
		}
		fin, changed := f.fin, f.change
		f.mu.Unlock()
		if amount > 0 {
			if err := f.send(&execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Credit{Credit: uint32(amount)}}); err != nil && !errors.Is(err, io.EOF) {
				return n, err
			}
		}
		if n > 0 {
			return n, nil
		}
		if err := context.Cause(f.ctx); err != nil {
			return 0, err
		}
		if fin {
			return 0, io.EOF
		}
		select {
		case <-f.ctx.Done():
		case <-changed:
		}
	}
}

// available waits before reading the source so unread bytes can stay in
// the TCP receive buffer. There is exactly one data sender per direction.
func (f *portFlow) available() (int, error) {
	for {
		f.mu.Lock()
		credit, changed := f.credit, f.change
		f.mu.Unlock()
		if err := context.Cause(f.ctx); err != nil {
			return 0, err
		}
		if credit != 0 {
			return credit, nil
		}
		select {
		case <-f.ctx.Done():
		case <-changed:
		}
	}
}

func (f *portFlow) data(p []byte) error {
	f.mu.Lock()
	f.credit -= len(p)
	f.mu.Unlock()
	return f.send(&execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Data{Data: p}})
}

func (f *portFlow) closeWrite() error {
	return f.send(&execenvv1.PortFrame{Payload: &execenvv1.PortFrame_CloseWrite{CloseWrite: true}})
}

// ServePort shares the credit protocol between both boundaries.
func ServePort(ctx context.Context, stream portStream, open func(*execenvv1.PortOpen) (execenv.PortConn, error)) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	request := first.GetOpen()
	if request == nil || !portconn.Valid(int(request.GetPort())) {
		return Error(execenv.ErrInvalid)
	}
	conn, err := open(request)
	if err != nil {
		return Error(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(execenv.ErrClosed)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if c, ok := conn.(interface {
		Done() <-chan struct{}
		Err() error
	}); ok {
		go func() {
			select {
			case <-ctx.Done():
			case <-c.Done():
				if err := c.Err(); err != nil {
					cancel(err)
				}
			}
		}()
	}
	if err := stream.Send(&execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Opened{Opened: true}}); err != nil {
		return err
	}
	flow := newPortFlow(ctx, stream)
	go func() {
		if err := flow.receive(); err != nil {
			var rpcErr *connect.Error
			if errors.As(err, &rpcErr) || errors.Is(err, io.EOF) {
				err = execenv.ErrConnection
			}
			cancel(err)
		}
	}()
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		if _, err := io.CopyBuffer(struct{ io.Writer }{conn}, portInput{flow}, make([]byte, portconn.ChunkSize)); err != nil {
			cancel(err)
		} else if err := conn.CloseWrite(); err != nil {
			cancel(err)
		}
	}()
	err = flow.copyFrom(conn)
	if err == nil {
		select {
		case <-inputDone:
		case <-ctx.Done():
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	if err != nil {
		cancel(err)
		<-inputDone
	}
	return Error(err)
}

type portInput struct{ *portFlow }

func (p portInput) Read(b []byte) (int, error) { return p.read(b) }

func (f *portFlow) copyFrom(conn execenv.PortConn) error {
	buf := make([]byte, portconn.ChunkSize)
	for {
		n, err := f.available()
		if err != nil {
			return err
		}
		n, err = conn.Read(buf[:min(n, len(buf))])
		if n > 0 {
			if sendErr := f.data(buf[:n]); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return f.closeWrite()
		}
		if err != nil {
			return err
		}
	}
}

// OpenPort bounds the handshake without shortening the life of the stream.
func OpenPort(ctx, parent context.Context, timeout time.Duration, dial func(context.Context) *portClient, request *execenvv1.PortOpen, mapError func(error) error) (execenv.PortConn, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(parent, func() { cancel(execenv.ErrConnection) })
	timer := time.AfterFunc(timeout, func() { cancel(execenv.ErrConnection) })
	stream := dial(ctx)
	err := stream.Send(&execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Open{Open: request}})
	if err == nil || errors.Is(err, io.EOF) {
		var ack *execenvv1.PortFrame
		ack, err = stream.Receive()
		if err == nil && !ack.GetOpened() {
			err = execenv.ErrConnection
		}
	}
	timer.Stop()
	if err != nil || ctx.Err() != nil {
		cause := context.Cause(ctx)
		stop()
		cancel(execenv.ErrClosed)
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
		if cause != nil {
			return nil, cause
		}
		return nil, mapError(err)
	}
	c := &framedPort{portFlow: newPortFlow(ctx, stream), stream: stream, port: int(request.GetPort()), cancel: cancel, done: make(chan struct{}), mapError: mapError}
	go func() {
		defer close(c.done)
		defer stop()
		defer stream.CloseResponse()
		if err := c.receive(); err != nil {
			cancel(mapError(err))
		}
	}()
	return c, nil
}

type framedPort struct {
	*portFlow
	stream   *portClient
	port     int
	cancel   context.CancelCauseFunc
	done     chan struct{}
	writeMu  sync.Mutex
	writeFIN atomic.Bool
	mapError func(error) error
}

func (c *framedPort) Port() int { return c.port }

func (c *framedPort) Read(p []byte) (int, error) {
	n, err := c.read(p)
	if errors.Is(err, io.EOF) && c.writeFIN.Load() {
		<-c.done
		if cause := context.Cause(c.ctx); cause != nil {
			return n, cause
		}
	}
	return n, c.transportError(err)
}

func (c *framedPort) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeFIN.Load() {
		return 0, execenv.ErrClosed
	}
	n := 0
	for len(p) > 0 {
		credit, err := c.available()
		if err != nil {
			return n, err
		}
		size := min(len(p), credit, portconn.ChunkSize)
		if err := c.data(p[:size]); err != nil {
			return n, c.sendError(err)
		}
		n += size
		p = p[size:]
	}
	return n, nil
}

func (c *framedPort) sendError(err error) error {
	if errors.Is(err, io.EOF) {
		// Connect exposes a terminal error through Receive.
		<-c.done
		if cause := context.Cause(c.ctx); cause != nil {
			return cause
		}
		return execenv.ErrClosed
	}
	return c.transportError(err)
}

func (c *framedPort) transportError(err error) error {
	if cause := context.Cause(c.ctx); cause != nil {
		return cause
	}
	var rpcErr *connect.Error
	if errors.As(err, &rpcErr) {
		return c.mapError(err)
	}
	return err
}

func (c *framedPort) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeFIN.Load() {
		return nil
	}
	if err := c.closeWrite(); err != nil {
		return c.sendError(err)
	}
	c.writeFIN.Store(true)
	c.mu.Lock()
	fin := c.fin
	c.mu.Unlock()
	if fin {
		<-c.done
	}
	return context.Cause(c.ctx)
}

func (c *framedPort) Close() error {
	c.cancel(execenv.ErrClosed)
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.stream.CloseRequest()
	return nil
}
