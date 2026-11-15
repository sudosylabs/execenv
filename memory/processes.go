package memory

import (
	"context"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/processes"
)

// processLog stands in for the guest helper's process recording: the fake
// shell reports itself and every command line it runs. PIDs are assigned
// per environment and never reused.
type processLog struct {
	log *processes.Log
	mu  sync.Mutex
	pid int
}

func newProcessLog() *processLog { return &processLog{log: processes.New(), pid: 1} }

func (p *processLog) nextPID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pid++
	return p.pid
}

// start records a started program and returns its PID. tag is the
// process's own when Start created it, else "".
func (p *processLog) start(parent int, args []string, tag string) int {
	pid := p.nextPID()
	argv, truncated := processes.Argv(args)
	p.log.Append(execenv.ProcessEvent{Kind: execenv.ProcessStarted, PID: pid, ParentPID: parent, Argv: argv, Truncated: truncated, Dir: execenv.GuestWorkspace, Tag: tag, At: time.Now()})
	return pid
}

func (p *processLog) exit(pid, code int) {
	p.log.Append(processes.Exited(pid, uint32(code)<<8, time.Now()))
}

func (p *processLog) killed(pid, signal int) {
	p.log.Append(processes.Exited(pid, uint32(signal), time.Now()))
}

func (e *environment) ObserveProcesses(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ProcessPage, error) {
	var page execenv.ProcessPage
	err := e.control.Run(ctx, fence, func() error {
		var err error
		page, err = e.processes.log.Page(after, limit)
		return err
	})
	return page, err
}

func (e *environment) AcknowledgeProcesses(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	return e.control.Run(ctx, fence, func() error {
		return e.processes.log.Acknowledge(through)
	})
}
