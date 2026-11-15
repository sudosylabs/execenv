package isolated

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/sudosylabs/execenv"
)

// monitor is the machine monitor's API on its socket in the jail. This file
// and the launcher are the only places that know its paths.
type monitor struct{ client *http.Client }

func newMonitor(sock string) *monitor {
	return &monitor{client: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}}
}

func (m *monitor) close() { m.client.CloseIdleConnections() }

func (m *monitor) do(ctx context.Context, method, path string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://monitor"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return execenv.ErrUnavailable
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return execenv.ErrUnavailable
	}
	return nil
}

func (m *monitor) patch(ctx context.Context, path string, body any) error {
	return m.do(ctx, http.MethodPatch, path, body)
}

func (m *monitor) put(ctx context.Context, path string, body any) error {
	return m.do(ctx, http.MethodPut, path, body)
}

// waitSocket waits until the monitor's API socket accepts a connection.
func waitSocket(ctx context.Context, sock string) error {
	for {
		if _, err := os.Stat(sock); err == nil {
			var d net.Dialer
			if conn, err := d.DialContext(ctx, "unix", sock); err == nil {
				_ = conn.Close()
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(execenv.ErrUnavailable, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The monitor's saved-machine files, inside the jail root.
const (
	jailState  = "vmstate"
	jailMemory = "memory"
)

// pauseMachine stops the machine's virtual CPUs through the monitor, which
// a saved machine needs; a signal-stopped monitor is continued first so it
// can answer.
func (m *monitor) pauseMachine(ctx context.Context) error {
	return m.patch(ctx, "/vm", map[string]any{"state": "Paused"})
}

func (m *monitor) resumeMachine(ctx context.Context) error {
	return m.patch(ctx, "/vm", map[string]any{"state": "Resumed"})
}

// save writes the paused machine's state and memory into the jail root.
func (m *monitor) save(ctx context.Context) error {
	return m.put(ctx, "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": inside(jailState),
		"mem_file_path": inside(jailMemory),
	})
}

// load restores a saved machine, paused, from the jail root. Its memory is
// mapped privately: pages it writes are its own, and the file stays as it
// was for the next grant.
func (m *monitor) load(ctx context.Context) error {
	return m.put(ctx, "/snapshot/load", map[string]any{
		"snapshot_path": inside(jailState),
		"mem_backend": map[string]any{
			"backend_type": "File",
			"backend_path": inside(jailMemory),
		},
		"enable_diff_snapshots": false,
		"resume_vm":             false,
	})
}

// rescanDisk makes the guest see the grant's workspace disk at its own
// size: the saved machine knew another.
func (m *monitor) rescanDisk(ctx context.Context) error {
	return m.patch(ctx, "/drives/workspace", map[string]any{
		"drive_id":     "workspace",
		"path_on_host": inside(jailDisk),
	})
}
