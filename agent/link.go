package agent

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/sudosylabs/execenv"
)

// protocol is the major version of proto/execenv/guest/v1. The host refuses
// a helper on any other.
const protocol = 1

// The guest link is one connection carrying HTTP/2 with prior knowledge.
// Its windows are wide: the link is local to the machine, and terminal
// output already has its own bounded history on the host.
const (
	linkStreamWindow     = 1 << 20
	linkConnectionWindow = 16 << 20
)

// watchedConn closes done once the connection has failed or been closed,
// so both ends learn the link is gone without a call in flight.
type watchedConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func watch(conn net.Conn) *watchedConn {
	return &watchedConn{Conn: conn, done: make(chan struct{})}
}

func (c *watchedConn) gone() { c.once.Do(func() { close(c.done) }) }

func (c *watchedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.gone()
	}
	return n, err
}

func (c *watchedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		c.gone()
	}
	return n, err
}

func (c *watchedConn) Close() error {
	err := c.Conn.Close()
	c.gone()
	return err
}

// oneConn is a listener that yields one connection, then waits for it to
// end. It lets an HTTP/2 server serve exactly the guest link.
type oneConn struct {
	conn *watchedConn
	used bool
	mu   sync.Mutex
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.conn.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return l.conn.LocalAddr() }

// linkServer serves handler on the one connection until it ends.
func linkServer(handler http.Handler) *http.Server {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:   handler,
		Protocols: protocols,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams:          1000,
			MaxReceiveBufferPerStream:     linkStreamWindow,
			MaxReceiveBufferPerConnection: linkConnectionWindow,
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
}

// linkClient speaks HTTP/2 over the one connection. A broken link is not
// redialled: a new link would be a new helper session.
func linkClient(conn *watchedConn) (*http.Client, *http.Transport) {
	var mu sync.Mutex
	used := false
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if used {
				return nil, execenv.ErrConnection
			}
			used = true
			return conn, nil
		},
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams:          1000,
			StrictMaxConcurrentRequests:   true,
			MaxReceiveBufferPerStream:     linkStreamWindow,
			MaxReceiveBufferPerConnection: linkConnectionWindow,
			// No health pings: a frozen machine cannot answer them, and a
			// frozen grant must keep its link. The monitor's exit and the
			// connection's own failure are what end it.
		},
	}
	return &http.Client{Transport: transport}, transport
}

// linkBase is the URL the generated clients address. The host part is
// never resolved: every request goes over the one connection.
const linkBase = "http://guest"
