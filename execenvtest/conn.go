package execenvtest

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/sudosylabs/execenv"
)

// ShellConn is a started shell as a byte stream: Write types at its input
// offset, Read returns its output and io.EOF after its exit. Tests that
// drive a shell by bytes use it.
type ShellConn struct {
	env     execenv.Env
	tag     string
	a       execenv.Attachment
	mu      sync.Mutex
	fence   execenv.ProjectionFence
	input   uint64
	pending []byte
}

// OpenShell starts a login shell tagged tag and attaches to it.
func OpenShell(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, tag string) *ShellConn {
	t.Helper()
	s := StartShell(t, env, fence, tag)
	return &ShellConn{env: env, tag: tag, a: s.a, fence: fence}
}

// SetFence moves the shell to the grant's current fence.
func (c *ShellConn) SetFence(fence execenv.ProjectionFence) {
	c.mu.Lock()
	c.fence = fence
	c.mu.Unlock()
}

func (c *ShellConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next, err := c.env.WriteInput(context.Background(), c.fence, c.tag, c.input, p)
	if next < c.input {
		return 0, err
	}
	n := int(next - c.input)
	c.input = next
	return n, err
}

func (c *ShellConn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		out, err := c.a.Next(context.Background())
		if err != nil {
			return 0, err
		}
		if out.Kind == execenv.OutputExit {
			return 0, io.EOF
		}
		c.pending = out.Data
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}
