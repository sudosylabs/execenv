// Package workload contains the guest workload behind a kernel-acknowledged
// write barrier. The control agent never belongs to that workload.
package workload

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
)

const transitionTimeout = 5 * time.Second

type freezer interface {
	write(string, string) error
	read(string) ([]byte, error)
}

type gate struct {
	mu     sync.Mutex
	fs     freezer
	failed bool
}

// stopped leaves an uncertain workload contained. A failed effect or failed
// transition poisons this occupancy; a retry may not run on a partially changed
// filesystem. Recovery requires retiring the environment.
func (g *gate) stopped(ctx context.Context, effect func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failed {
		return execenv.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.transition(ctx, true); err != nil {
		g.failed = true
		return err
	}
	if err := effect(); err != nil {
		g.failed = true
		return err
	}
	if err := g.transition(ctx, false); err != nil {
		g.failed = true
		// Reassert containment even if the request deadline expired during recovery.
		return errors.Join(err, g.fs.write("cgroup.freeze", "1"))
	}
	return nil
}

func (g *gate) transition(ctx context.Context, frozen bool) error {
	value := 0
	if frozen {
		value = 1
	}
	if err := g.fs.write("cgroup.freeze", strconv.Itoa(value)); err != nil {
		return execenv.ErrUnavailable
	}
	waitCtx, cancel := context.WithTimeout(ctx, transitionTimeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := g.fs.read("cgroup.events")
		if err != nil {
			return execenv.ErrUnavailable
		}
		state, ok := eventValue(data, "frozen")
		if !ok {
			return execenv.ErrUnavailable
		}
		if state == value {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func eventValue(raw []byte, key string) (int, bool) {
	found := false
	result := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != key {
			continue
		}
		if found || len(fields) != 2 || fields[1] != "0" && fields[1] != "1" {
			return 0, false
		}
		found = true
		result = int(fields[1][0] - '0')
	}
	return result, found
}
