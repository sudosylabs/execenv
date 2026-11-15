// Package journal is a grant's projection journal over a real directory:
// the caller's tree projected into it, and the changes made there reported
// back until the caller acknowledges them. The guest agent runs it on the
// workspace disk; the in-memory host runs it on a temporary directory, so
// both serve the same contract from the same code.
package journal

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/observations"
	"github.com/sudosylabs/execenv/internal/projection"
	"github.com/sudosylabs/execenv/internal/tree"
)

// Barrier stops every writer under the journal's directory while fn runs.
// An error from fn means the disk state is uncertain; the barrier stays
// closed after it.
type Barrier interface {
	Stopped(ctx context.Context, fn func() error) error
}

// Journal is serialized by its caller, independently of any RPC
// connection. Its barrier must contain every guest writer before capture and
// commit. An uncertain disk mutation poisons the barrier until guest teardown.
type Journal struct {
	home     string
	store    *content.Store
	barrier  Barrier
	state    *projection.State
	log      *observations.Log
	snapshot *guesttree.Snapshot
	fence    execenv.ProjectionFence
	own      func(string) error
}

// New journals home, keeping bodies in store, which must not be under home.
// own, when set, hands every projected path to the workload's user.
func New(home string, store *content.Store, barrier Barrier, own func(string) error) *Journal {
	return &Journal{home: home, store: store, barrier: barrier, state: projection.New(), own: own}
}

// Receive stores a body. It needs no lock: Upload then registers it.
func (j *Journal) Receive(body io.Reader) (content.Blob, error) {
	return j.store.Put(body)
}

// sweep drops every stored body nothing references any more.
func (j *Journal) sweep() {
	live := make(map[string]bool)
	j.state.Live(live)
	if j.log != nil {
		j.log.Live(live)
	}
	_ = j.store.Sweep(live)
}

// Initialized reports whether the projection has been initialized.
func (j *Journal) Initialized() bool { return j.state.Initialized() }

// Body opens the retained observation body ref names.
func (j *Journal) Body(fence execenv.ProjectionFence, ref execenv.ContentReference) (*os.File, error) {
	if err := j.Bind(fence); err != nil {
		return nil, err
	}
	blob, err := j.log.Content(ref)
	if err != nil {
		return nil, err
	}
	return j.store.Open(blob)
}
func (j *Journal) Close() { j.snapshot.Close() }
func (j *Journal) Bind(fence execenv.ProjectionFence) error {
	if execenv.ValidateProjectionFence(fence) != nil {
		return execenv.ErrInvalid
	}
	if j.fence.EnvironmentEpoch != "" && (j.fence.EnvironmentEpoch != fence.EnvironmentEpoch || j.fence.ExecutionGrantID != fence.ExecutionGrantID || j.fence.FenceRevision > fence.FenceRevision) {
		return execenv.ErrConflict
	}
	j.fence = fence
	if j.log == nil {
		j.log = observations.New(fence.ExecutionGrantID, fence.EnvironmentEpoch)
	}
	return nil
}

// Upload registers a body Receive stored.
func (j *Journal) Upload(fence execenv.ProjectionFence, blob content.Blob) (execenv.ContentReference, error) {
	if err := j.Bind(fence); err != nil {
		return execenv.ContentReference{}, err
	}
	return j.state.Put(blob)
}
func (j *Journal) Initialize(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	if err := j.Bind(request.Fence); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	var receipt execenv.ProjectionReceipt
	var resultErr error
	err := j.barrier.Stopped(ctx, func() error {
		touched := false
		receipt, resultErr = j.state.Initialize(request, func(snapshot tree.Tree) error {
			current, err := guesttree.Capture(j.home, nil, nil)
			if err != nil {
				return err
			}
			defer current.Close()
			if len(current.Nodes) != 0 {
				return execenv.ErrConflict
			}
			next, err := tree.FromTree(snapshot)
			if err != nil {
				return err
			}
			touched = true
			if err := rewriteHome(j.store, j.home, next); err != nil {
				return err
			}
			return j.ownTree(nil)
		})
		if resultErr != nil {
			if touched {
				return resultErr
			}
			return nil
		}
		if j.snapshot == nil {
			return j.captureProjection(nil)
		}
		return nil
	})
	if err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if resultErr == nil {
		// Consumed uploads are no longer needed.
		j.sweep()
	}
	return receipt, resultErr
}
func (j *Journal) Apply(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	if err := j.Bind(request.Fence); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	var receipt execenv.ProjectionReceipt
	var resultErr error
	err := j.barrier.Stopped(ctx, func() error {
		if !j.state.Initialized() {
			resultErr = execenv.ErrConflict
			return nil
		}
		if err := j.observeCaptured(); err != nil {
			return err
		}
		touched := false
		var disk tree.Batch
		receipt, resultErr = j.state.Apply(request, func(plan projection.Plan) error {
			prepared, err := plan.Prepare(j.snapshot)
			if err != nil {
				return err
			}
			disk = prepared.Batch
			if len(disk.Mutations) == 0 {
				return nil
			}
			touched = true
			if err := applyHome(j.store, j.home, disk); err != nil {
				return err
			}
			roots := make([]string, 0, len(disk.Mutations))
			for _, m := range disk.Mutations {
				roots = append(roots, m.Path)
			}
			return j.ownTree(roots)
		})
		if resultErr != nil {
			if touched {
				return resultErr
			}
			return nil
		}
		// An exact replay performs no disk work and must preserve all later guest
		// observations. Only this commit's known effects update identity bindings.
		if touched {
			return j.captureProjection(disk.Mutations)
		}
		return nil
	})
	if err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if resultErr == nil {
		// Consumed uploads are no longer needed.
		j.sweep()
	}
	return receipt, resultErr
}
func (j *Journal) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	if err := j.Bind(fence); err != nil {
		return execenv.ObservationPage{}, err
	}
	if !j.state.Initialized() {
		return execenv.ObservationPage{}, execenv.ErrConflict
	}
	if err := j.barrier.Stopped(ctx, j.observeCaptured); err != nil {
		return execenv.ObservationPage{}, err
	}
	return j.log.Page(after, limit)
}
func (j *Journal) observeCaptured() error {
	next, err := guesttree.CaptureInto(j.home, j.snapshot, j.observePolicy(), j.store)
	if err != nil {
		j.log.Lagged()
		return err
	}
	changes, err := guesttree.Diff(j.snapshot, next)
	if err != nil {
		next.Close()
		j.log.Lagged()
		return err
	}
	versions := make(map[string]execenv.Version)
	for _, node := range j.state.Nodes() {
		versions[node.Identity] = node.Version
	}
	captured := make([]observations.Captured, 0, len(changes))
	for _, change := range changes {
		event := execenv.ExecutionObservation{Fence: j.fence, BasedOnWorkspaceCursor: j.state.Cursor(), Operation: execenv.ObservationOperation(change.Operation), NodeIdentity: change.Identity, Kind: change.Kind, Path: change.Path, DestinationPath: change.Destination, ExpectedContentVersion: versions[change.Identity]}
		if change.Operation == guesttree.CreateFile || change.Operation == guesttree.CreateDirectory {
			event.ExpectedContentVersion = ""
		}
		if change.Operation == guesttree.Delete && change.Kind == execenv.ProjectionDirectory {
			recursive := change.Recursive
			event.Recursive = &recursive
		}
		var body content.Blob
		if change.Operation == guesttree.CreateFile || change.Operation == guesttree.WriteFile {
			// The guest is stopped, so the captured file still holds what
			// was hashed. Keep a copy: the event's body must not change.
			if body, err = j.keep(change); err != nil {
				next.Close()
				j.log.Lagged()
				return err
			}
		}
		captured = append(captured, observations.Captured{Event: event, Content: body})
	}
	if err := j.log.Append(captured); err != nil {
		next.Close()
		return err
	}
	for range changes {
		if err := j.state.GuestChanged(); err != nil {
			next.Close()
			return err
		}
	}
	j.snapshot.Close()
	j.snapshot = next
	return nil
}
func (j *Journal) captureProjection(mutations []tree.Mutation) error {
	// The guest is stopped, so the workspace is the previous capture plus this
	// commit's effects. With ignore rules, admit exactly that: a path a
	// projected ignore-file edit has just made visible must reach the stream
	// through the next observation, not slip silently into the baseline.
	var observe guesttree.Observe
	if j.state.Ignore() != nil {
		observe = func(name string, _, held bool) bool { return held || j.state.Holds(name) }
	}
	next, err := guesttree.Capture(j.home, j.snapshot, observe)
	if err != nil {
		return err
	}
	// Rebind only exact projected path/kind/content effects. Unrelated captured
	// guest nodes keep their physical identities and pending observation bytes.
	for name, node := range j.state.Nodes() {
		affected := mutations == nil
		for _, m := range mutations {
			for _, root := range []string{m.Path, m.From} {
				if root != "" && (name == root || strings.HasPrefix(name, root+"/")) {
					affected = true
				}
			}
		}
		if !affected {
			continue
		}
		actual, ok := next.Nodes[name]
		if ok && actual.Kind == node.Kind && actual.Content == node.Content {
			actual.Identity = node.Identity
			next.Nodes[name] = actual
		}
	}
	j.snapshot.Close()
	j.snapshot = next
	return nil
}

// observePolicy returns the capture filter for the ignore file as the guest
// left it, or nil when nothing is ignored. Projected paths are held.
func (j *Journal) observePolicy() guesttree.Observe {
	rules := j.state.Ignore()
	if rules == nil {
		return nil
	}
	body, present := readIgnoreFile(j.home, rules.File())
	policy := rules.Policy(body, present)
	if !policy.Active() {
		return nil
	}
	return func(name string, dir, held bool) bool {
		return policy.Observed(name, dir, held || j.state.Holds(name))
	}
}

// readIgnoreFile reads at most one byte past the limit, so an oversized file
// reaches the policy as not parsing. A missing, unreadable or non-regular
// file is absent.
func readIgnoreFile(home, name string) ([]byte, bool) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, execenv.MaxIgnoreFileBytes+1))
	if err != nil {
		return nil, false
	}
	return body, true
}

// ownTree hands projected paths to the guest user. roots nil means the whole
// workspace (initialization); otherwise only the subtrees this commit wrote,
// so ignored guest trees elsewhere are never walked.
func (j *Journal) ownTree(roots []string) error {
	if j.own == nil {
		return nil
	}
	walk := func(start string) error {
		return filepath.WalkDir(start, func(name string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return j.own(name)
		})
	}
	if roots == nil {
		return walk(j.home)
	}
	for _, root := range roots {
		full, err := execenv.ResolvePath(j.home, root)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(full); os.IsNotExist(err) {
			continue
		}
		if err := walk(full); err != nil {
			return err
		}
	}
	return nil
}
func (j *Journal) Content(fence execenv.ProjectionFence, reference execenv.ContentReference) (io.ReadCloser, error) {
	return j.Body(fence, reference)
}

// keep copies a captured file's body into the store, refusing one that no
// longer matches its capture.
func (j *Journal) keep(change guesttree.Change) (content.Blob, error) {
	if j.store.Has(change.Content) {
		return change.Content, nil
	}
	file := change.File
	if file == nil {
		return content.Blob{}, execenv.ErrLagged
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return content.Blob{}, execenv.ErrLagged
	}
	blob, err := j.store.Put(file)
	if err != nil {
		return content.Blob{}, err
	}
	if blob != change.Content {
		return content.Blob{}, execenv.ErrLagged
	}
	return blob, nil
}
func (j *Journal) Acknowledge(fence execenv.ProjectionFence, through uint64) error {
	if err := j.Bind(fence); err != nil {
		return err
	}
	if err := j.log.Acknowledge(through); err != nil {
		return err
	}
	j.sweep()
	return nil
}

func (j *Journal) Confirm(binding execenv.ObservationProjection) error {
	if err := j.Bind(binding.Fence); err != nil {
		return err
	}
	return j.state.RegisterObservation(binding, j.log.Event)
}
