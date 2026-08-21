package memory

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/tree"
)

const watchBuffer = 32

type observation struct {
	env    *environment
	mu     sync.Mutex
	cursor execenv.Cursor
	events chan execenv.Event
	err    error
	once   sync.Once
}

// ReplaceTree converges the projection to tree. See execenv.Node for the
// nil-Data version-skip rule: that avoids resending bodies the host already
// has after a reconnect.
func (e *environment) ReplaceTree(ctx context.Context, snap execenv.Tree) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("replace", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("replace"); err != nil {
		return err
	}
	next, err := tree.Replace(e.files, snap)
	if err != nil {
		return execenv.Error("replace", err)
	}
	e.files = next
	e.failWatch(execenv.ErrLagged)
	e.log.Reset()
	return nil
}

// Apply applies mutations on a copy so a late conflict cannot leave a
// half-applied tree.
func (e *environment) Apply(ctx context.Context, batch execenv.Batch) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("apply", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("apply"); err != nil {
		return err
	}
	next, err := tree.Apply(e.files, batch)
	if err != nil {
		return execenv.Error("apply", err)
	}
	e.files = next
	return nil
}

func (e *environment) Watch(ctx context.Context, after execenv.Cursor) (execenv.Observation, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("watch", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("watch"); err != nil {
		return nil, err
	}
	if e.obs != nil {
		return nil, execenv.Error("watch", execenv.ErrBusy)
	}
	cursor, replay, err := e.log.Since(after)
	if err != nil {
		return nil, execenv.Error("watch", err)
	}
	obs := &observation{
		env:    e,
		cursor: cursor,
		events: make(chan execenv.Event, max(watchBuffer, len(replay))),
	}
	for _, event := range replay {
		obs.events <- event
	}
	e.obs = obs
	return obs, nil
}

func (e *environment) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("open", err)
	}
	if err := execenv.ValidatePath(path); err != nil {
		return nil, execenv.Error("open", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("open"); err != nil {
		return nil, err
	}
	ent, ok := e.files[path]
	if !ok || ent.Kind != execenv.KindFile {
		return nil, execenv.Error("open", execenv.ErrNotFound)
	}
	body := append([]byte(nil), ent.Data...)
	return io.NopCloser(bytes.NewReader(body)), nil
}

// WriteGuest implements execenv.GuestWriter. It is how tests pretend the
// isolated environment created a file. The write is visible to Watch and Open
// and is not a caller Apply.
func (h *Host) WriteGuest(ctx context.Context, id execenv.ID, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(path); err != nil {
		return execenv.Error("guest", err)
	}
	if int64(len(data)) > execenv.MaxFileBytes {
		return execenv.Error("guest", execenv.ErrTooLarge)
	}
	env, err := h.grant(id)
	if err != nil {
		return err
	}
	return env.writeGuest(path, data)
}

func (h *Host) RemoveGuest(ctx context.Context, id execenv.ID, path string) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(path); err != nil {
		return execenv.Error("guest", err)
	}
	env, err := h.grant(id)
	if err != nil {
		return err
	}
	return env.removeGuest(path)
}

func (h *Host) MoveGuest(ctx context.Context, id execenv.ID, from, to string) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(from); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(to); err != nil {
		return execenv.Error("guest", err)
	}
	env, err := h.grant(id)
	if err != nil {
		return err
	}
	return env.moveGuest(from, to)
}

func (h *Host) grant(id execenv.ID) (*environment, error) {
	h.mu.Lock()
	env, ok := h.grants[id]
	h.mu.Unlock()
	if !ok {
		return nil, execenv.Error("guest", execenv.ErrRevoked)
	}
	return env, nil
}

func (e *environment) writeGuest(path string, data []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("guest"); err != nil {
		return err
	}
	_, existed := e.files[path]
	e.files[path] = tree.Node{
		Kind:    execenv.KindFile,
		Version: "guest",
		Data:    append([]byte(nil), data...),
	}
	op := execenv.OpCreate
	if existed {
		op = execenv.OpReplace
	}
	e.emit(execenv.Event{Op: op, Path: path})
	return nil
}

func (e *environment) removeGuest(path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("guest"); err != nil {
		return err
	}
	if _, ok := e.files[path]; !ok {
		return execenv.Error("guest", execenv.ErrNotFound)
	}
	delete(e.files, path)
	e.emit(execenv.Event{Op: execenv.OpDelete, Path: path})
	return nil
}

func (e *environment) moveGuest(from, to string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("guest"); err != nil {
		return err
	}
	ent, ok := e.files[from]
	if !ok {
		return execenv.Error("guest", execenv.ErrNotFound)
	}
	if _, exists := e.files[to]; exists {
		return execenv.Error("guest", execenv.ErrConflict)
	}
	delete(e.files, from)
	e.files[to] = ent
	e.emit(execenv.Event{Op: execenv.OpMove, Path: to, From: from})
	return nil
}

func (e *environment) guard(op string) error {
	if e.revoked {
		return execenv.Error(op, execenv.ErrRevoked)
	}
	if e.frozen {
		return execenv.Error(op, execenv.ErrFrozen)
	}
	return nil
}

func (e *environment) emit(event execenv.Event) {
	event = e.log.Append(event)
	if e.obs == nil {
		return
	}
	select {
	case e.obs.events <- event:
	default:
		e.failWatch(execenv.ErrLagged)
	}
}

func (e *environment) failWatch(err error) {
	if e.obs == nil {
		return
	}
	e.obs.fail(err)
	e.obs = nil
}

func (o *observation) Cursor() execenv.Cursor {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cursor
}

func (o *observation) Next(ctx context.Context) (execenv.Event, error) {
	o.mu.Lock()
	failed := o.err
	o.mu.Unlock()
	if failed != nil {
		return execenv.Event{}, execenv.Error("watch", failed)
	}
	select {
	case <-ctx.Done():
		return execenv.Event{}, execenv.Error("watch", ctx.Err())
	case ev, ok := <-o.events:
		if !ok {
			o.mu.Lock()
			err := o.err
			o.mu.Unlock()
			if err == nil {
				err = execenv.ErrClosed
			}
			return execenv.Event{}, execenv.Error("watch", err)
		}
		o.mu.Lock()
		if o.err != nil {
			err := o.err
			o.mu.Unlock()
			return execenv.Event{}, execenv.Error("watch", err)
		}
		o.cursor = ev.Cursor
		o.mu.Unlock()
		return ev, nil
	}
}

func (o *observation) fail(err error) {
	o.once.Do(func() {
		o.mu.Lock()
		o.err = err
		o.mu.Unlock()
		close(o.events)
	})
}

func (o *observation) Close() error {
	o.env.mu.Lock()
	defer o.env.mu.Unlock()
	if o.env.obs == o {
		o.env.failWatch(execenv.ErrClosed)
	}
	return nil
}
