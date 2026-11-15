package memory

import (
	"context"

	"github.com/sudosylabs/execenv"
)

// Processes: starts and input are fenced; reads need a live grant.

func (e *environment) Start(ctx context.Context, fence execenv.ProjectionFence, spec execenv.ProcessSpec) (execenv.ProcessInfo, error) {
	var info execenv.ProcessInfo
	err := e.control.Run(ctx, fence, func() error {
		var err error
		info, err = e.procs.Start(ctx, spec)
		return err
	})
	return info, err
}

func (e *environment) Process(ctx context.Context, tag string) (execenv.ProcessInfo, error) {
	if err := e.live("process"); err != nil {
		return execenv.ProcessInfo{}, err
	}
	return e.procs.Process(tag)
}

func (e *environment) ListProcesses(ctx context.Context) ([]execenv.ProcessInfo, error) {
	if err := e.live("processes"); err != nil {
		return nil, err
	}
	return e.procs.List()
}

func (e *environment) AttachProcess(ctx context.Context, tag string, from map[execenv.Stream]uint64) (execenv.Attachment, error) {
	if err := e.live("attach"); err != nil {
		return nil, err
	}
	return e.procs.Attach(tag, from)
}

func (e *environment) WriteInput(ctx context.Context, fence execenv.ProjectionFence, tag string, offset uint64, data []byte) (uint64, error) {
	var next uint64
	err := e.control.Run(ctx, fence, func() error {
		var err error
		next, err = e.procs.WriteInput(tag, offset, data)
		return err
	})
	return next, err
}

func (e *environment) CloseInput(ctx context.Context, fence execenv.ProjectionFence, tag string) error {
	return e.control.Run(ctx, fence, func() error { return e.procs.CloseInput(tag) })
}

func (e *environment) Resize(ctx context.Context, fence execenv.ProjectionFence, tag string, w execenv.Window) error {
	return e.control.Run(ctx, fence, func() error { return e.procs.Resize(ctx, tag, w) })
}

func (e *environment) SignalProcess(ctx context.Context, fence execenv.ProjectionFence, tag string, s execenv.Signal) error {
	return e.control.Run(ctx, fence, func() error { return e.procs.Signal(tag, s) })
}
