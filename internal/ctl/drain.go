package ctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/sudosylabs/execenv/daemon"
)

// operatorSocketName is the daemon's operator socket in the state directory.
const operatorSocketName = "operator.sock"

// DefaultDrainTimeout is how long execenvctl drain waits by default.
const DefaultDrainTimeout = 15 * time.Minute

// drainPoll is how often drain re-reads the live grant count.
var drainPoll = time.Second

// errGrantsLive refuses a restart that would destroy live grants.
var errGrantsLive = errors.New("grants are live")

// errImageInUse refuses to remove an image live grants were started from.
var errImageInUse = errors.New("image is in use")

func (o Options) operatorSocket() string { return filepath.Join(o.State, operatorSocketName) }

// socketPath is the operator socket the host config names. Empty means the
// config names none.
func (o Options) socketPath() string {
	doc, err := loadHost(o.configPath())
	if err != nil {
		return ""
	}
	return doc.OperatorSocket
}

func (o Options) operate(op daemon.OperatorOp) (daemon.OperatorReply, error) {
	path := o.socketPath()
	if path == "" {
		return daemon.OperatorReply{}, fmt.Errorf("host config names no operator_socket; rerun execenvctl bootstrap")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return daemon.Operate(ctx, path, op)
}

func (o Options) unitActive() bool {
	if o.UnitActive != nil {
		return o.UnitActive()
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", unitName).Run() == nil
}

// guardRestart refuses to restart a daemon whose grants would not survive
// it, while it holds any, unless Force is set. A daemon whose grants are
// durable adopts them again, so restarting it costs none.
func (o Options) guardRestart() error {
	if o.Force {
		return nil
	}
	reply, err := o.operate(daemon.OperatorStatus)
	return restartVerdict(o.unitActive(), reply, err)
}

// restartVerdict decides whether a restart may proceed without --force.
// A daemon that answers may restart only while it holds no grants, whether
// or not it is draining. One that does not answer may restart only when its
// unit is not running, since then nothing is restarted at all.
func restartVerdict(active bool, reply daemon.OperatorReply, err error) error {
	if err != nil {
		if !active {
			return nil
		}
		return fmt.Errorf("cannot ask the running daemon whether grants are live (%v); pass --force to restart anyway", err)
	}
	if reply.Grants > 0 && !reply.Durable {
		if reply.State == daemon.DrainServing {
			return fmt.Errorf("%w: the daemon holds %d; run execenvctl drain first, or pass --force to destroy them", errGrantsLive, reply.Grants)
		}
		return fmt.Errorf("%w: the draining daemon still holds %d; wait for execenvctl drain, or pass --force to destroy them", errGrantsLive, reply.Grants)
	}
	return nil
}

// imageInUse says a durable daemon's live grants run from image id. A
// daemon whose grants end with a restart has its refusal in guardRestart.
func imageInUse(reply daemon.OperatorReply, id string) bool {
	if !reply.Durable {
		return false
	}
	for _, used := range reply.Images {
		if used == id {
			return true
		}
	}
	return false
}

// Drain tells the local daemon to stop occupying new grants, then waits
// until it holds none or timeout passes, printing the remaining count as it
// changes. A zero timeout starts draining and returns at once. A drained
// daemon keeps refusing new grants until Undrain or a restart.
func Drain(opts Options, timeout time.Duration, stdout io.Writer) error {
	opts = opts.resolved()
	if stdout == nil {
		stdout = io.Discard
	}
	reply, err := opts.operate(daemon.OperatorDrain)
	if err != nil {
		return wrap("drain", err)
	}
	deadline := time.Now().Add(timeout)
	last := -1
	for {
		if reply.Grants != last {
			fmt.Fprintf(stdout, "state=%s grants=%d\n", reply.State, reply.Grants)
			last = reply.Grants
		}
		if reply.Grants == 0 {
			return nil
		}
		if timeout == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return wrap("drain", fmt.Errorf("timed out with %d grants remaining; the daemon stays draining", reply.Grants))
		}
		time.Sleep(min(drainPoll, time.Until(deadline)))
		if reply, err = opts.operate(daemon.OperatorStatus); err != nil {
			return wrap("drain", err)
		}
	}
}

// Undrain tells the local daemon to occupy new grants again.
func Undrain(opts Options, stdout io.Writer) error {
	opts = opts.resolved()
	reply, err := opts.operate(daemon.OperatorUndrain)
	if err != nil {
		return wrap("undrain", err)
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "state=%s grants=%d\n", reply.State, reply.Grants)
	}
	return nil
}

// daemonSummary is the drain state for status output.
func daemonSummary(opts Options) string {
	reply, err := opts.operate(daemon.OperatorStatus)
	if err != nil {
		return "drain=unreachable"
	}
	return fmt.Sprintf("drain=%s\ngrants=%d", reply.State, reply.Grants)
}
