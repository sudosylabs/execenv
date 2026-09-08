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

type writeBarrier interface {
	Stopped(context.Context, func() error) error
}

// diskJournal is serialized by the agent server, independently of any RPC
// connection. Its barrier must contain every guest writer before capture and
// commit. An uncertain disk mutation poisons the barrier until guest teardown.
type diskJournal struct {
	home     string
	barrier  writeBarrier
	state    *projection.State
	log      *observations.Log
	snapshot *guesttree.Snapshot
	fence    execenv.ProjectionFence
	own      func(string) error
}

func newDiskJournal(home string, barrier writeBarrier, own func(string) error) *diskJournal {
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
			current, err := guesttree.Capture(j.home, nil)
			if err != nil {
				return err
			}
			defer current.Close()
			if len(current.Nodes) != 0 {
				return execenv.ErrConflict
			}
			next, err := tree.Replace(nil, snapshot)
			if err != nil {
				return err
			}
			touched = true
			if err := rewriteHome(j.home, next); err != nil {
				return err
			}
			return j.ownTree()
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
			return j.ownTree()
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
	next, err := guesttree.Capture(j.home, j.snapshot)
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
	next, err := guesttree.Capture(j.home, j.snapshot)
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
func (j *diskJournal) ownTree() error {
	if j.own == nil {
		return nil
	}
	return filepath.WalkDir(j.home, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return j.own(name)
	})
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
