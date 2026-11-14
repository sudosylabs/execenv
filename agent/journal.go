package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/observations"
	"github.com/sudosylabs/execenv/internal/projection"
	"github.com/sudosylabs/execenv/internal/tree"
)

// diskJournal is serialized by the agent server, independently of any RPC
// connection. Its barrier must contain every guest writer before capture and
// commit. An uncertain disk mutation poisons the barrier until guest teardown.
type diskJournal struct {
	home     string
	barrier  WriteBarrier
	state    *projection.State
	log      *observations.Log
	snapshot *guesttree.Snapshot
	fence    execenv.ProjectionFence
	own      func(string) error
}

func newDiskJournal(home string, barrier WriteBarrier, own func(string) error) *diskJournal {
	return &diskJournal{home: home, barrier: barrier, state: projection.New(), own: own}
}
func (j *diskJournal) close() { j.snapshot.Close() }
func (j *diskJournal) bind(fence execenv.ProjectionFence) error {
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
func (j *diskJournal) upload(fence execenv.ProjectionFence, body []byte) (execenv.ContentReference, error) {
	if err := j.bind(fence); err != nil {
		return execenv.ContentReference{}, err
	}
	return j.state.Put(body)
}
func (j *diskJournal) initialize(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	if err := j.bind(request.Fence); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	var receipt execenv.ProjectionReceipt
	var resultErr error
	err := j.barrier.Stopped(ctx, func() error {
		touched := false
		receipt, resultErr = j.state.Initialize(request, func(snapshot execenv.Tree) error {
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
			if err := rewriteHome(j.home, next); err != nil {
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
	return receipt, resultErr
}
func (j *diskJournal) apply(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	if err := j.bind(request.Fence); err != nil {
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
		var disk execenv.Batch
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
			if err := applyHome(j.home, disk); err != nil {
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
	return receipt, resultErr
}
func (j *diskJournal) observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	if err := j.bind(fence); err != nil {
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
func (j *diskJournal) observeCaptured() error {
	next, err := guesttree.Capture(j.home, j.snapshot, j.observePolicy())
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
		var body []byte
		if change.Operation == guesttree.CreateFile || change.Operation == guesttree.WriteFile {
			body = change.Data
		}
		captured = append(captured, observations.Captured{Event: event, Data: body})
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
func (j *diskJournal) captureProjection(mutations []execenv.Mutation) error {
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
		if ok && actual.Kind == node.Kind && bytes.Equal(actual.Data, node.Data) {
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
func (j *diskJournal) observePolicy() guesttree.Observe {
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
func (j *diskJournal) ownTree(roots []string) error {
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
func (j *diskJournal) content(fence execenv.ProjectionFence, reference execenv.ContentReference) (io.ReadCloser, error) {
	if err := j.bind(fence); err != nil {
		return nil, err
	}
	body, err := j.log.Content(reference)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}
func (j *diskJournal) acknowledge(fence execenv.ProjectionFence, through uint64) error {
	if err := j.bind(fence); err != nil {
		return err
	}
	return j.log.Acknowledge(through)
}

func (j *diskJournal) confirm(binding execenv.ObservationProjection) error {
	if err := j.bind(binding.Fence); err != nil {
		return err
	}
	return j.state.RegisterObservation(binding, j.log.Event)
}
