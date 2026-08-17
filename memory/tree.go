package memory

import (
	"bytes"
	"context"
	"io"
	"maps"

	"github.com/sudosylabs/execenv"
)

const watchBuffer = 32

type observation struct {
	env    *environment
	events chan execenv.Event
}

// ReplaceTree converges the projection to tree. See execenv.Node for the
// nil-Data version-skip rule: that avoids resending bodies the host already
// has after a reconnect.
func (e *environment) ReplaceTree(ctx context.Context, tree execenv.Tree) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("replace", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("replace"); err != nil {
		return err
	}
	next, err := replaceInto(e.files, tree)
	if err != nil {
		return execenv.Error("replace", err)
	}
	e.files = next
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
	next, err := applyInto(e.files, batch)
	if err != nil {
		return execenv.Error("apply", err)
	}
	e.files = next
	return nil
}

func (e *environment) Watch(ctx context.Context) (execenv.Observation, error) {
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
	obs := &observation{
		env:    e,
		events: make(chan execenv.Event, watchBuffer),
	}
	e.obs = obs
	e.watchErr = nil
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
	if !ok || ent.kind != execenv.KindFile {
		return nil, execenv.Error("open", execenv.ErrNotFound)
	}
	// Copy so the caller cannot race with a later Apply.
	body := append([]byte(nil), ent.data...)
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
	e.files[path] = node{
		kind:    execenv.KindFile,
		version: "guest",
		data:    append([]byte(nil), data...),
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
	if e.obs == nil {
		return
	}
	select {
	case e.obs.events <- event:
	default:
		// Fail closed: a slow consumer must ReplaceTree rather than
		// apply a gap. Do not drop the event and continue.
		e.failWatch(execenv.ErrLagged)
	}
}

func (e *environment) failWatch(err error) {
	if e.obs == nil {
		return
	}
	e.watchErr = err
	close(e.obs.events)
	e.obs = nil
}

func (o *observation) Next(ctx context.Context) (execenv.Event, error) {
	select {
	case <-ctx.Done():
		return execenv.Event{}, execenv.Error("watch", ctx.Err())
	case ev, ok := <-o.events:
		if !ok {
			o.env.mu.Lock()
			err := o.env.watchErr
			o.env.mu.Unlock()
			if err == nil {
				err = execenv.ErrClosed
			}
			return execenv.Event{}, execenv.Error("watch", err)
		}
		return ev, nil
	}
}

func (o *observation) Close() error {
	o.env.mu.Lock()
	defer o.env.mu.Unlock()
	if o.env.obs == o {
		o.env.failWatch(execenv.ErrClosed)
	}
	return nil
}

func replaceInto(current map[string]node, tree execenv.Tree) (map[string]node, error) {
	if len(tree) > execenv.MaxTreeEntries {
		return nil, execenv.ErrTooLarge
	}
	var total int64
	seen := make(map[string]struct{}, len(tree))
	next := make(map[string]node, len(tree))
	for _, item := range tree {
		if err := execenv.ValidatePath(item.Path); err != nil {
			return nil, err
		}
		if _, dup := seen[item.Path]; dup {
			return nil, execenv.ErrInvalid
		}
		seen[item.Path] = struct{}{}
		switch item.Kind {
		case execenv.KindDirectory:
			if item.Version != "" || item.Data != nil {
				return nil, execenv.ErrInvalid
			}
			next[item.Path] = node{kind: execenv.KindDirectory}
		case execenv.KindFile:
			body, err := resolveBody(current[item.Path], item)
			if err != nil {
				return nil, err
			}
			total += int64(len(body))
			if int64(len(body)) > execenv.MaxFileBytes || total > execenv.MaxTreeBytes {
				return nil, execenv.ErrTooLarge
			}
			next[item.Path] = node{kind: execenv.KindFile, version: item.Version, data: body}
		default:
			return nil, execenv.ErrInvalid
		}
	}
	return next, nil
}

// resolveBody implements the nil-Data skip. The host never invents bytes:
// if the caller omitted the body, we must already have that exact version.
func resolveBody(existing node, item execenv.Node) ([]byte, error) {
	if item.Data != nil {
		out := append([]byte(nil), item.Data...)
		return out, nil
	}
	if existing.kind != execenv.KindFile || existing.version == "" || existing.version != item.Version {
		return nil, execenv.ErrInvalid
	}
	return append([]byte(nil), existing.data...), nil
}

func applyInto(current map[string]node, batch execenv.Batch) (map[string]node, error) {
	if len(batch.Mutations) > execenv.MaxTreeEntries {
		return nil, execenv.ErrTooLarge
	}
	next := maps.Clone(current)
	if next == nil {
		next = make(map[string]node)
	}
	var total int64
	for _, mut := range batch.Mutations {
		if err := applyOne(next, mut, &total); err != nil {
			return nil, err
		}
	}
	return next, nil
}

func applyOne(next map[string]node, mut execenv.Mutation, total *int64) error {
	if err := execenv.ValidatePath(mut.Path); err != nil {
		return err
	}
	switch mut.Op {
	case execenv.OpCreate:
		if _, ok := next[mut.Path]; ok {
			return execenv.ErrConflict
		}
		return put(next, mut, total)
	case execenv.OpReplace:
		ent, ok := next[mut.Path]
		if !ok || ent.kind != execenv.KindFile {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.version != mut.Expected {
			return execenv.ErrConflict
		}
		return put(next, mut, total)
	case execenv.OpDelete:
		ent, ok := next[mut.Path]
		if !ok {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.kind == execenv.KindFile && ent.version != mut.Expected {
			return execenv.ErrConflict
		}
		delete(next, mut.Path)
		return nil
	case execenv.OpMove:
		if err := execenv.ValidatePath(mut.From); err != nil {
			return err
		}
		ent, ok := next[mut.From]
		if !ok {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.kind == execenv.KindFile && ent.version != mut.Expected {
			return execenv.ErrConflict
		}
		if _, exists := next[mut.Path]; exists {
			return execenv.ErrConflict
		}
		delete(next, mut.From)
		next[mut.Path] = ent
		return nil
	default:
		return execenv.ErrInvalid
	}
}

func put(next map[string]node, mut execenv.Mutation, total *int64) error {
	switch mut.Kind {
	case execenv.KindDirectory:
		if mut.Data != nil || mut.Version != "" {
			return execenv.ErrInvalid
		}
		next[mut.Path] = node{kind: execenv.KindDirectory}
		return nil
	case execenv.KindFile:
		if mut.Data == nil {
			return execenv.ErrInvalid
		}
		if int64(len(mut.Data)) > execenv.MaxFileBytes {
			return execenv.ErrTooLarge
		}
		*total += int64(len(mut.Data))
		if *total > execenv.MaxTreeBytes {
			return execenv.ErrTooLarge
		}
		next[mut.Path] = node{
			kind:    execenv.KindFile,
			version: mut.Version,
			data:    append([]byte(nil), mut.Data...),
		}
		return nil
	default:
		return execenv.ErrInvalid
	}
}
