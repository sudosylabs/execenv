// Package portconn manages loopback connections and their grant lifetimes.
package portconn

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sudosylabs/execenv"
)

const DialTimeout = 5 * time.Second

func Valid(port int) bool { return port >= 1 && port <= 65535 }

type tcpConn struct {
	*net.TCPConn
	port int
}

func (c tcpConn) Port() int { return c.port }

func (c tcpConn) Read(p []byte) (int, error) {
	n, err := c.TCPConn.Read(p)
	return n, socketError(err)
}

func (c tcpConn) Write(p []byte) (int, error) {
	n, err := c.TCPConn.Write(p)
	return n, socketError(err)
}

func (c tcpConn) CloseWrite() error { return socketError(c.TCPConn.CloseWrite()) }

func socketError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return execenv.ErrConnection
}

// Dial tries only literal loopback addresses. The memory adapter selects
// which families its registered listeners can actually accept.
func Dial(ctx context.Context, port int, ipv4, ipv6 bool) (execenv.PortConn, error) {
	dialer := net.Dialer{}
	return dialLoopback(ctx, port, ipv4, ipv6, DialTimeout, dialer.DialContext)
}

func dialLoopback(ctx context.Context, port int, ipv4, ipv6 bool, timeout time.Duration, dial func(context.Context, string, string) (net.Conn, error)) (execenv.PortConn, error) {
	if !Valid(port) {
		return nil, execenv.ErrInvalid
	}
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, execenv.ErrConnection)
	defer cancel()
	for _, target := range []struct {
		enabled bool
		network string
		host    string
	}{{ipv4, "tcp4", "127.0.0.1"}, {ipv6, "tcp6", "::1"}} {
		if !target.enabled {
			continue
		}
		conn, err := dial(ctx, target.network, net.JoinHostPort(target.host, strconv.Itoa(port)))
		switch {
		case err == nil:
			return tcpConn{conn.(*net.TCPConn), port}, nil
		case ctx.Err() != nil:
			return nil, context.Cause(ctx)
		case errors.Is(err, syscall.ECONNREFUSED):
			continue
		case target.network == "tcp6" && (errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.ENETUNREACH)):
			continue // This guest has no IPv6 loopback.
		default:
			return nil, execenv.ErrConnection
		}
	}
	return nil, execenv.ErrRefused
}

// Set tracks dialing and open connections that one grant can end together.
type Set struct {
	mu      sync.Mutex
	conns   map[*Slot]struct{}
	limit   execenv.TokenBucket
	buckets [2]bucket
	changed chan struct{}
}

// Reserve takes a slot under the grant's control gate. Dial must run
// outside that gate. release ends the idle hold when the slot closes.
func (s *Set) Reserve(ctx context.Context, release func()) (*Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) >= execenv.MaxPortConns {
		return nil, execenv.ErrBusy
	}
	ctx, cancel := context.WithCancelCause(ctx)
	c := &Slot{set: s, ctx: ctx, cancel: cancel, release: release, done: make(chan struct{})}
	if s.conns == nil {
		s.conns = make(map[*Slot]struct{})
	}
	s.conns[c] = struct{}{}
	c.stop = context.AfterFunc(ctx, func() { c.end(context.Cause(ctx)) })
	return c, nil
}

func (s *Set) End(cause error) {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	for c := range conns {
		c.markEndedLocked(cause)
	}
	s.mu.Unlock()
	for c := range conns {
		c.close()
	}
}

// Slot is one reserved or established connection.
type Slot struct {
	set     *Set
	ctx     context.Context
	cancel  context.CancelCauseFunc
	stop    func() bool
	release func()
	once    sync.Once
	done    chan struct{}

	// Guarded by set.mu. conn and mapError become immutable once assigned.
	conn      execenv.PortConn
	mapError  func(error) error
	cause     error
	readDone  bool
	writeDone bool
}

func (c *Slot) Dial(dial func(context.Context) (execenv.PortConn, error), mapError func(error) error) (execenv.PortConn, error) {
	conn, err := dial(c.ctx)
	c.set.mu.Lock()
	if cause := context.Cause(c.ctx); cause != nil {
		err = cause
	}
	if err == nil {
		c.conn, c.mapError = conn, mapError
		c.set.mu.Unlock()
		return c, nil
	}
	c.set.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	if mapError != nil {
		err = mapError(err)
	}
	c.end(err)
	return nil, c.Err()
}

func (c *Slot) Port() int             { return c.conn.Port() }
func (c *Slot) Done() <-chan struct{} { return c.done }

func (c *Slot) Err() error {
	c.set.mu.Lock()
	defer c.set.mu.Unlock()
	return c.cause
}

func (c *Slot) close() {
	c.once.Do(func() {
		c.stop()
		c.set.mu.Lock()
		conn := c.conn
		c.set.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		c.set.mu.Lock()
		delete(c.set.conns, c)
		c.set.mu.Unlock()
		c.release()
		close(c.done)
		c.cancel(execenv.ErrClosed)
	})
}

func (c *Slot) markEndedLocked(cause error) {
	if c.cause == nil {
		c.cause = cause
		c.cancel(cause)
	}
}

func (c *Slot) end(cause error) {
	c.set.mu.Lock()
	c.markEndedLocked(cause)
	c.set.mu.Unlock()
	c.close()
}

func (c *Slot) half(read bool) {
	c.set.mu.Lock()
	if read {
		c.readDone = true
	} else {
		c.writeDone = true
	}
	done := c.readDone && c.writeDone
	c.set.mu.Unlock()
	if done {
		c.close()
	}
}

func (c *Slot) failure(err error) error {
	if cause := c.Err(); cause != nil {
		return cause
	}
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	if c.mapError != nil {
		err = c.mapError(err)
	}
	if !errors.Is(err, execenv.ErrClosed) {
		c.end(err)
	}
	if cause := c.Err(); cause != nil {
		return cause
	}
	return err
}

func (c *Slot) status(dir direction) error {
	c.set.mu.Lock()
	defer c.set.mu.Unlock()
	if c.cause != nil {
		return c.cause
	}
	if dir == fromGuest && c.readDone {
		return io.EOF
	}
	if dir == toGuest && c.writeDone {
		return execenv.ErrClosed
	}
	return nil
}

func (c *Slot) Read(p []byte) (int, error) {
	if err := c.status(fromGuest); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := c.conn.Read(p[:min(len(p), ChunkSize)])
	// Reading again grants new wire credit. Hold this read until the
	// budget covers its bytes, so throttled streams cannot refill credit.
	for left := n; left > 0; {
		allowed, limitErr := c.allow(fromGuest, left)
		if limitErr != nil {
			return 0, limitErr
		}
		left -= allowed
	}
	if err = c.failure(err); errors.Is(err, io.EOF) {
		c.half(true)
	}
	return n, err
}

func (c *Slot) Write(p []byte) (int, error) {
	if err := c.status(toGuest); err != nil {
		return 0, err
	}
	written := 0
	for len(p) != 0 {
		allowed, err := c.allow(toGuest, min(len(p), ChunkSize))
		if err != nil {
			return written, err
		}
		n, err := c.conn.Write(p[:allowed])
		written += n
		if err := c.failure(err); err != nil {
			return written, err
		}
		if n != allowed {
			return written, c.failure(execenv.ErrConnection)
		}
		p = p[n:]
	}
	return written, nil
}

func (c *Slot) CloseWrite() error {
	if err := c.Err(); err != nil {
		return err
	}
	err := c.failure(c.conn.CloseWrite())
	if err == nil {
		c.half(false)
	}
	return err
}

func (c *Slot) Close() error {
	c.end(execenv.ErrClosed)
	return nil
}
