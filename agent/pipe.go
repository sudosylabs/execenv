package agent

import (
	"errors"
	"net"
	"os"
	"time"
)

// NewPipeConn joins a read pipe and a write pipe into one link. A sandbox
// runtime that has no guest socket family carries the host link on the
// agent's standard input and output instead. Deadlines apply when the
// platform supports them on pipes and are otherwise ignored.
func NewPipeConn(r, w *os.File) net.Conn { return &pipeConn{r: r, w: w} }

type pipeConn struct {
	r, w *os.File
}

func (c *pipeConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *pipeConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *pipeConn) Close() error                { return errors.Join(c.r.Close(), c.w.Close()) }
func (c *pipeConn) LocalAddr() net.Addr         { return pipeAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr        { return pipeAddr{} }
func (c *pipeConn) SetDeadline(t time.Time) error {
	return errors.Join(c.r.SetReadDeadline(t), c.w.SetWriteDeadline(t))
}
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "stdio" }
