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

const portChunk = 32 << 10

// MaxPortMessage leaves room for a data frame's protobuf overhead.
const MaxPortMessage = portChunk + 256

type portStream interface {
	Send(*execenvv1.PortFrame) error
	Receive() (*execenvv1.PortFrame, error)
}

type portClient = connect.BidiStreamForClient[execenvv1.PortFrame, execenvv1.PortFrame]

func portData(data []byte) *execenvv1.PortFrame {
	return &execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Data{Data: data}}
}

func portEOF() *execenvv1.PortFrame {
	return &execenvv1.PortFrame{Payload: &execenvv1.PortFrame_CloseWrite{CloseWrite: true}}
}

// ServePort shares the handshake and byte relay between both boundaries.
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
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := stream.Send(&execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Opened{Opened: true}}); err != nil {
		return err
	}

	// A grant can end after its listener has already sent EOF.
	held, _ := conn.(interface {
		Done() <-chan struct{}
		Err() error
	})
	var ended <-chan struct{}
	if held != nil {
		ended = held.Done()
	}

	input := make(chan error, 1)
	go func() {
		err := receivePortInput(stream, conn)
		input <- err
		if err != nil {
			_ = conn.Close()
		}
	}()

	buf := make([]byte, portChunk)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(portData(buf[:n])); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			if err := stream.Send(portEOF()); err != nil {
				return err
			}
			select {
			case err := <-input:
				return Error(err)
			case <-ended:
				return Error(held.Err())
			case <-ctx.Done():
				return Error(ctx.Err())
			}
		}
		if err != nil {
			select {
			case inErr := <-input:
				if inErr != nil {
					return Error(inErr)
				}
			default:
			}
			return Error(err)
		}
	}
}

func receivePortInput(stream portStream, conn execenv.PortConn) error {
	for {
		frame, err := stream.Receive()
		if err != nil {
			return err
		}
		if frame.GetCloseWrite() {
			return conn.CloseWrite()
		}
		data := frame.GetData()
		if len(data) == 0 || len(data) > portChunk {
			return execenv.ErrInvalid
		}
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
	}
}

// OpenPort bounds the handshake separately from the stream's lifetime.
// parent is the lifetime of the client that owns the transport.
func OpenPort(ctx, parent context.Context, timeout time.Duration, dial func(context.Context) *portClient, request *execenvv1.PortOpen, mapError func(error) error) (execenv.PortConn, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(parent, func() { cancel(execenv.ErrConnection) })
	timer := time.AfterFunc(timeout, func() { cancel(context.DeadlineExceeded) })
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

	r, w := io.Pipe()
	c := &framedPort{stream: stream, port: int(request.GetPort()), read: r, cancel: cancel, ctx: ctx, mapError: mapError, done: make(chan struct{})}
	unblock := context.AfterFunc(ctx, func() { _ = w.CloseWithError(context.Cause(ctx)) })
	go func() {
		defer close(c.done)
		defer stop()
		defer unblock()
		defer stream.CloseResponse()
		err := receivePortOutput(stream, w, &c.readFIN)
		if err != nil {
			if ctx.Err() == nil {
				cancel(mapError(err))
			}
			_ = w.CloseWithError(context.Cause(ctx))
		} else {
			_ = w.Close()
		}
	}()
	return c, nil
}

func receivePortOutput(stream portStream, w *io.PipeWriter, fin *atomic.Bool) error {
	readEOF := false
	for {
		frame, err := stream.Receive()
		if errors.Is(err, io.EOF) && readEOF {
			return nil
		}
		if err != nil {
			return err
		}
		if readEOF {
			return execenv.ErrConnection
		}
		if frame.GetCloseWrite() {
			readEOF = true
			fin.Store(true)
			_ = w.Close()
			continue
		}
		data := frame.GetData()
		if len(data) == 0 || len(data) > portChunk {
			return execenv.ErrConnection
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
}

type framedPort struct {
	stream   *portClient
	port     int
	read     *io.PipeReader
	ctx      context.Context
	cancel   context.CancelCauseFunc
	mapError func(error) error
	done     chan struct{}

	writeMu  sync.Mutex
	readFIN  atomic.Bool
	writeFIN atomic.Bool
}

func (c *framedPort) Port() int { return c.port }

func (c *framedPort) Read(p []byte) (int, error) {
	if err := context.Cause(c.ctx); err != nil {
		return 0, err
	}
	n, err := c.read.Read(p)
	if errors.Is(err, io.EOF) && c.writeFIN.Load() {
		<-c.done
	}
	if cause := context.Cause(c.ctx); cause != nil {
		return n, cause
	}
	return n, err
}

func (c *framedPort) writeError(err error) error {
	if err == nil {
		return nil
	}
	if cause := context.Cause(c.ctx); cause != nil {
		return cause
	}
	if errors.Is(err, io.EOF) {
		// Connect reports a server error through Receive. The read pump
		// publishes it before it exits.
		<-c.done
		if cause := context.Cause(c.ctx); cause != nil {
			return cause
		}
		return execenv.ErrClosed
	}
	return c.mapError(err)
}

func (c *framedPort) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := context.Cause(c.ctx); err != nil {
		return 0, err
	}
	if c.writeFIN.Load() {
		return 0, execenv.ErrClosed
	}
	n := 0
	for len(p) > 0 {
		size := min(len(p), portChunk)
		if err := c.stream.Send(portData(p[:size])); err != nil {
			return n, c.writeError(err)
		}
		n += size
		p = p[size:]
	}
	return n, nil
}

func (c *framedPort) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := context.Cause(c.ctx); err != nil {
		return err
	}
	if c.writeFIN.Load() {
		return nil
	}
	if err := c.stream.Send(portEOF()); err != nil {
		return c.writeError(err)
	}
	if err := c.writeError(c.stream.CloseRequest()); err != nil {
		return err
	}
	c.writeFIN.Store(true)
	if c.readFIN.Load() {
		<-c.done
	}
	return context.Cause(c.ctx)
}

func (c *framedPort) Close() error {
	c.cancel(execenv.ErrClosed)
	_ = c.read.Close()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.stream.CloseRequest()
	return nil
}

func ListeningPortsTo(ports []execenv.ListeningPort) *execenvv1.ListeningPortsResponse {
	resp := &execenvv1.ListeningPortsResponse{}
	for _, port := range ports {
		resp.Ports = append(resp.Ports, &execenvv1.ListeningPort{Port: int32(port.Port), Address: port.Address, Tag: port.Tag})
	}
	return resp
}

func ListeningPortsFrom(resp *execenvv1.ListeningPortsResponse) []execenv.ListeningPort {
	var ports []execenv.ListeningPort
	for _, port := range resp.GetPorts() {
		ports = append(ports, execenv.ListeningPort{Port: int(port.GetPort()), Address: port.GetAddress(), Tag: port.GetTag()})
	}
	return ports
}
