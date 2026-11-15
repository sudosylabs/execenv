package isolated

import (
	"context"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/procs"
)

// Processes: the host keeps each process's history and input offsets; the
// guest runs it. Starts and input are fenced; reads need a live grant.

// startProcess is the manager's Starter: one process in the guest.
func (e *environment) startProcess(ctx context.Context, spec execenv.ProcessSpec) (procs.Backend, error) {
	client, err := e.live("start")
	if err != nil {
		return nil, err
	}
	b, err := client.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	return guestProcess{Backend: b, env: e, link: client.Done()}, nil
}

// guestProcess waits for the verdict when its guest link broke under it,
// so its readers see ErrLost or ErrRevoked rather than a killed process.
type guestProcess struct {
	procs.Backend
	env  *environment
	link <-chan struct{}
}

func (p guestProcess) Wait() execenv.ExitStatus {
	status := p.Backend.Wait()
	select {
	case <-p.link:
		_ = p.env.endCause(nil)
	default:
	}
	return status
}

func (e *environment) Start(ctx context.Context, fence execenv.ProjectionFence, spec execenv.ProcessSpec) (execenv.ProcessInfo, error) {
	var info execenv.ProcessInfo
	err := e.control.Run(ctx, fence, func() error {
		var err error
		info, err = e.manager().Start(ctx, spec)
		return err
	})
	return info, err
}

func (e *environment) Process(_ context.Context, tag string) (execenv.ProcessInfo, error) {
	if _, err := e.live("process"); err != nil {
		return execenv.ProcessInfo{}, err
	}
	return e.manager().Process(tag)
}

func (e *environment) ListProcesses(context.Context) ([]execenv.ProcessInfo, error) {
	if _, err := e.live("processes"); err != nil {
		return nil, err
	}
	return e.manager().List()
}

func (e *environment) AttachProcess(_ context.Context, tag string, from map[execenv.Stream]uint64) (execenv.Attachment, error) {
	if _, err := e.live("attach"); err != nil {
		return nil, err
	}
	return e.manager().Attach(tag, from)
}

func (e *environment) WriteInput(ctx context.Context, fence execenv.ProjectionFence, tag string, offset uint64, data []byte) (uint64, error) {
	var next uint64
	err := e.control.Run(ctx, fence, func() error {
		var err error
		next, err = e.manager().WriteInput(tag, offset, data)
		return err
	})
	return next, err
}

func (e *environment) CloseInput(ctx context.Context, fence execenv.ProjectionFence, tag string) error {
	return e.control.Run(ctx, fence, func() error { return e.manager().CloseInput(tag) })
}

func (e *environment) Resize(ctx context.Context, fence execenv.ProjectionFence, tag string, w execenv.Window) error {
	return e.control.Run(ctx, fence, func() error { return e.manager().Resize(ctx, tag, w) })
}

func (e *environment) SignalProcess(ctx context.Context, fence execenv.ProjectionFence, tag string, s execenv.Signal) error {
	return e.control.Run(ctx, fence, func() error { return e.manager().Signal(tag, s) })
}

// manager is the grant's process manager: a suspend replaces it.
func (e *environment) manager() *procs.Manager {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.procs
}
