package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
)

// The operator socket is how execenvctl on the same machine drains the
// daemon before a restart. It is a Unix socket, not the grant wire: grant
// callers authenticate to the remote listener and must not be able to take
// a host out of service, while the operator tool already runs as the user
// that owns the daemon's state directory. File permissions (0600, owned by
// the daemon user) authenticate it, it never leaves the machine, and one
// request and one reply per connection carry the live grant count back,
// which a signal and a state file could not do without a second file and
// a race. Nothing on it names a grant, a token or any terminal output.

// OperatorOp is one request on the operator socket.
type OperatorOp string

const (
	// OperatorStatus reports the drain state and live grant count.
	OperatorStatus OperatorOp = "status"
	// OperatorDrain stops occupying new grants and keeps serving held ones.
	OperatorDrain OperatorOp = "drain"
	// OperatorUndrain occupies new grants again.
	OperatorUndrain OperatorOp = "undrain"
)

// DrainState is the daemon's drain state machine:
//
//	serving --drain--> draining --last grant ends--> drained
//	draining, drained --undrain--> serving
//
// A drained daemon still refuses new grants; it is only safe to restart.
// A restarted daemon starts serving: drain state is not persisted.
type DrainState string

const (
	DrainServing  DrainState = "serving"
	DrainDraining DrainState = "draining"
	DrainDrained  DrainState = "drained"
)

// OperatorReply is the answer to every operator request.
type OperatorReply struct {
	State DrainState `json:"state"`
	// Grants is how many grants the daemon holds, including starting ones.
	Grants int `json:"grants"`
	// Durable says the grants survive a restart of the daemon.
	Durable bool `json:"durable"`
	// Images are the catalog images live grants were started from.
	Images []string `json:"images,omitempty"`
	// Error is set when the request failed.
	Error string `json:"error,omitempty"`
}

// drainable is the operator side of a host adapter. Both adapters the
// daemon composes implement it; it is not part of the execenv.Host contract.
type drainable interface {
	SetDraining(on bool)
	// ImagesInUse are the images live grants were started from.
	ImagesInUse() []execenv.Image
	// DurableGrants says grants survive a restart of the daemon.
	DurableGrants() bool
}

// drain owns the daemon's drain state over one host.
type drain struct {
	host     execenv.Host
	adapter  drainable
	mu       sync.Mutex
	draining bool
}

func newDrain(host execenv.Host) (*drain, error) {
	adapter, ok := host.(drainable)
	if !ok {
		return nil, execenv.Error("operator", execenv.ErrInvalid)
	}
	return &drain{host: host, adapter: adapter}, nil
}

// do applies op and reports the resulting state.
func (d *drain) do(ctx context.Context, op OperatorOp) (OperatorReply, error) {
	d.mu.Lock()
	switch op {
	case OperatorStatus:
	case OperatorDrain:
		d.draining = true
		d.adapter.SetDraining(true)
	case OperatorUndrain:
		d.draining = false
		d.adapter.SetDraining(false)
	default:
		d.mu.Unlock()
		return OperatorReply{}, execenv.ErrInvalid
	}
	draining := d.draining
	d.mu.Unlock()
	report, err := d.host.Ready(ctx)
	if err != nil {
		return OperatorReply{}, err
	}
	state := DrainServing
	switch {
	case draining && report.Grants == 0:
		state = DrainDrained
	case draining:
		state = DrainDraining
	}
	reply := OperatorReply{State: state, Grants: report.Grants, Durable: d.adapter.DurableGrants()}
	for _, image := range d.adapter.ImagesInUse() {
		reply.Images = append(reply.Images, string(image))
	}
	return reply, nil
}

// listenOperator binds the operator socket at path, replacing a stale socket
// a crashed daemon left behind but never any other kind of file.
func listenOperator(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != fs.ModeSocket {
			return nil, execenv.Error("operator", execenv.ErrInvalid)
		}
		if err := os.Remove(path); err != nil {
			return nil, execenv.Error("operator", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, execenv.Error("operator", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, execenv.Error("operator", err)
	}
	return ln, nil
}

// ServeOperator answers operator requests on ln for host until ctx ends.
// host must be one of the adapters the daemon composes.
func ServeOperator(ctx context.Context, ln net.Listener, host execenv.Host) error {
	d, err := newDrain(host)
	if err != nil {
		_ = ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return execenv.Error("operator", err)
		}
		go serveOperatorConn(ctx, conn, d)
	}
}

const operatorTimeout = 10 * time.Second

type operatorRequest struct {
	Op OperatorOp `json:"op"`
}

func serveOperatorConn(ctx context.Context, conn net.Conn, d *drain) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(operatorTimeout))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req operatorRequest
	var reply OperatorReply
	if err := json.Unmarshal(line, &req); err != nil {
		reply.Error = "invalid request"
	} else {
		opCtx, cancel := context.WithTimeout(ctx, operatorTimeout)
		reply, err = d.do(opCtx, req.Op)
		cancel()
		if err != nil {
			reply = OperatorReply{Error: err.Error()}
		}
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

// ErrNoOperator means nothing answered on the operator socket: the daemon
// is not running, or not yet listening.
var ErrNoOperator = errors.New("execenv: daemon not answering on the operator socket")

// Operate sends op to the daemon's operator socket at path.
func Operate(ctx context.Context, path string, op OperatorOp) (OperatorReply, error) {
	if path == "" {
		return OperatorReply{}, execenv.Error("operator", execenv.ErrInvalid)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return OperatorReply{}, execenv.Error("operator", ErrNoOperator)
	}
	defer conn.Close()
	deadline := time.Now().Add(operatorTimeout)
	if when, ok := ctx.Deadline(); ok && when.Before(deadline) {
		deadline = when
	}
	_ = conn.SetDeadline(deadline)
	raw, err := json.Marshal(operatorRequest{Op: op})
	if err != nil {
		return OperatorReply{}, execenv.Error("operator", err)
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return OperatorReply{}, execenv.Error("operator", ErrNoOperator)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return OperatorReply{}, execenv.Error("operator", ErrNoOperator)
	}
	var reply OperatorReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return OperatorReply{}, execenv.Error("operator", execenv.ErrInvalid)
	}
	if reply.Error != "" {
		return reply, execenv.Error("operator", errors.New(reply.Error))
	}
	return reply, nil
}
