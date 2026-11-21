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

	"github.com/sudosylabs/execenv"
)

func Valid(port int) bool { return port >= 1 && port <= 65535 }

type tcpConn struct {
	*net.TCPConn
	port int
}

func (c tcpConn) Port() int { return c.port }

// Dial uses a literal IPv4 loopback destination, with no name resolution.
func Dial(ctx context.Context, port int) (execenv.PortConn, error) {
	if !Valid(port) {
		return nil, execenv.ErrInvalid
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if errors.Is(err, syscall.ECONNREFUSED) {
		return nil, execenv.ErrRefused
	}
	if err != nil {
		return nil, err
	}
	return tcpConn{conn.(*net.TCPConn), port}, nil
}

// Set tracks the connections one grant can end together. The caller
// serializes Add and End with changes to the grant's running state.
type Set struct {
	mu      sync.Mutex
	conns   map[*held]struct{}
	limit   execenv.TokenBucket
	buckets [2]bucket
	changed chan struct{}
}

func (s *Set) Add(ctx context.Context, conn execenv.PortConn, mapError func(error) error) execenv.PortConn {
	c := &held{PortConn: conn, set: s, done: make(chan struct{}), mapError: mapError}
	s.mu.Lock()
	if s.conns == nil {
		s.conns = make(map[*held]struct{})
	}
	s.conns[c] = struct{}{}
	c.stop = context.AfterFunc(ctx, func() { c.end(ctx.Err()) })
	s.mu.Unlock()
	return c
}

func (s *Set) End(cause error) {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	for c := range conns {
		c.cause = cause
		close(c.done)
	}
	s.mu.Unlock()
	for c := range conns {
		c.close()
	}
}

type held struct {
	execenv.PortConn
	set      *Set
	stop     func() bool
	once     sync.Once
	done     chan struct{}
	mapError func(error) error

	// All state below is guarded by set.mu.
	cause     error
	readDone  bool
	writeDone bool
}

func (c *held) Done() <-chan struct{} { return c.done }

func (c *held) Err() error {
	c.set.mu.Lock()
	defer c.set.mu.Unlock()
	return c.cause
}

func (c *held) close() {
	c.once.Do(func() {
		c.stop()
		_ = c.PortConn.Close()
		c.set.mu.Lock()
		delete(c.set.conns, c)
		c.set.mu.Unlock()
	})
}

func (c *held) end(cause error) {
	c.set.mu.Lock()
	if c.cause == nil {
		c.cause = cause
		close(c.done)
	}
	c.set.mu.Unlock()
	c.close()
}

func (c *held) half(read bool) {
	c.set.mu.Lock()
	if read {
		c.readDone = true
	} else {
		c.writeDone = true
	}
	done := c.readDone && c.writeDone
	if done {
	}
	c.set.mu.Unlock()
	if done {
		c.close()
	}
}

func (c *held) failure(err error) error {
	if err != nil && c.mapError != nil {
		return c.mapError(err)
	}
	return err
}

func (c *held) status(dir direction) error {
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

func (c *held) Read(p []byte) (int, error) {
	if err := c.status(fromGuest); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := c.PortConn.Read(p[:min(len(p), ChunkSize)])
	for left := n; left > 0; {
		allowed, limitErr := c.allow(fromGuest, left)
		if limitErr != nil {
			return 0, limitErr
		}
		left -= allowed
	}
	if cause := c.Err(); cause != nil {
		return n, cause
	}
	if errors.Is(err, io.EOF) {
		c.half(true)
	}
	return n, c.failure(err)
}

func (c *held) Write(p []byte) (int, error) {
	if err := c.status(toGuest); err != nil {
		return 0, err
	}
	written := 0
	for len(p) != 0 {
		allowed, err := c.allow(toGuest, min(len(p), ChunkSize))
		if err != nil {
			return written, err
		}
		n, err := c.PortConn.Write(p[:allowed])
		written += n
		if cause := c.Err(); cause != nil {
			return written, cause
		}
		if err != nil {
			return written, c.failure(err)
		}
		if n != allowed {
			return written, io.ErrShortWrite
		}
		p = p[n:]
	}
	return written, nil
}

func (c *held) CloseWrite() error {
	if err := c.Err(); err != nil {
		return err
	}
	err := c.PortConn.CloseWrite()
	if cause := c.Err(); cause != nil {
		return cause
	}
	if err == nil {
		c.half(false)
	}
	return c.failure(err)
}

func (c *held) Close() error {
	c.end(execenv.ErrClosed)
	return nil
}
