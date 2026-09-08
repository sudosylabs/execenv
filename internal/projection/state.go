// Package projection owns immutable uploads, consecutive journal transactions,
// stable entry identities, and bounded idempotent receipts for one environment.
package projection

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"path"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
)

const retainedReceipts = 65536
const retainedUploads = 512

type Node struct {
	Identity string
	EntryID  string
	Kind     execenv.ProjectionKind
	Version  execenv.Version
	Data     []byte
}

type upload struct {
	reference execenv.ContentReference
	data      []byte
	expires   time.Time
}
type receipt struct {
	digest [32]byte
	value  execenv.ProjectionReceipt
}

// State is serialized by its owning environment, across all connections.
// Commit callbacks must atomically install the supplied disk mutations while
// holding the backend's guest-write barrier. Memory commits need no disk I/O.
type State struct {
	initialized         bool
	nodes               map[string]Node
	cursor              uint64
	hostCursor          uint64
	uploads             map[string]upload
	uploadBytes         int64
	receipts            map[string]receipt
	proofs              map[uint64]observationProof
	observationBindings map[uint64]observationProof
}

func New() *State {
	return &State{nodes: make(map[string]Node), uploads: make(map[string]upload), receipts: make(map[string]receipt), proofs: make(map[uint64]observationProof), observationBindings: make(map[uint64]observationProof)}
}
func (s *State) Initialized() bool  { return s.initialized }
func (s *State) Cursor() uint64     { return s.cursor }
func (s *State) HostCursor() uint64 { return s.hostCursor }

// Put copies bounded bytes and generates an unguessable, single-purpose handle.
// The environment's fence check binds this store to its exact host/grant/epoch.
func (s *State) Put(data []byte) (execenv.ContentReference, error) {
	for id, u := range s.uploads {
		if !time.Now().Before(u.expires) {
			delete(s.uploads, id)
			s.uploadBytes -= u.reference.Size
		}
	}
	if len(data) > execenv.MaxFileBytes || s.uploadBytes+int64(len(data)) > execenv.MaxTreeBytes || len(s.uploads) >= retainedUploads {
		return execenv.ContentReference{}, execenv.ErrTooLarge
	}
	sum := sha256.Sum256(data)
	ref := execenv.ContentReference{TransferID: rand.Text(), Size: int64(len(data)), Digest: hex.EncodeToString(sum[:])}
	s.uploads[ref.TransferID] = upload{reference: ref, data: append([]byte{}, data...), expires: time.Now().Add(5 * time.Minute)}
	s.uploadBytes += ref.Size
	return ref, nil
}

func (s *State) content(ref *execenv.ContentReference) ([]byte, error) {
	if ref == nil {
		return nil, execenv.ErrInvalid
	}
	upload, ok := s.uploads[ref.TransferID]
	if !ok || upload.reference != *ref || !time.Now().Before(upload.expires) {
		return nil, execenv.ErrConflict
	}
	return upload.data, nil
}

func (s *State) replay(id string, digest [32]byte) (execenv.ProjectionReceipt, bool, error) {
	old, ok := s.receipts[id]
	if !ok {
		return execenv.ProjectionReceipt{}, false, nil
	}
	if old.digest != digest {
		return execenv.ProjectionReceipt{}, true, execenv.ErrConflict
	}
	return old.value, true, nil
}
func (s *State) remember(id string, digest [32]byte, value execenv.ProjectionReceipt) {
	s.receipts[id] = receipt{digest: digest, value: value}

}
func (s *State) consume(refs []*execenv.ContentReference) {
	for _, ref := range refs {
		if ref != nil {
			if u, ok := s.uploads[ref.TransferID]; ok {
				s.uploadBytes -= u.reference.Size
				delete(s.uploads, ref.TransferID)
			}
		}
	}
}

func (s *State) Initialize(request execenv.ProjectionInitialization, commit func(execenv.Tree) error) (execenv.ProjectionReceipt, error) {
	if err := execenv.ValidateProjectionInitialization(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	raw, _ := json.Marshal(request)
	digest := sha256.Sum256(append([]byte("initialize:"), raw...))
	if old, ok, err := s.replay(request.MutationID, digest); ok {
		return old, err
	}
	if len(s.receipts) >= retainedReceipts {
		return execenv.ProjectionReceipt{}, execenv.ErrTooLarge
	}
	if s.initialized || s.hostCursor != 0 {
		return execenv.ProjectionReceipt{}, execenv.ErrConflict
	}
	next := make(map[string]Node)
	refs := make([]*execenv.ContentReference, 0, len(request.Entries))
	disk := make(execenv.Tree, 0, len(request.Entries))
	for _, entry := range request.Entries {
		mutation := execenv.ProjectionMutation{Operation: execenv.ProjectionCreate, EntryID: entry.EntryID, Kind: entry.Kind, Path: entry.Path, ResultingContentVersion: entry.ContentVersion, Content: entry.Content}
		if err := s.applyOne(next, mutation); err != nil {
			return execenv.ProjectionReceipt{}, err
		}
		refs = append(refs, entry.Content)
		node := next[entry.Path]
		disk = append(disk, execenv.Node{Path: entry.Path, Kind: nodeKind(node.Kind), Version: node.Version, Data: node.Data})
	}
	if err := validateSize(next); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if err := commit(disk); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	s.initialized = true
	s.nodes = next
	s.cursor = request.WorkspaceCursor
	value := execenv.ProjectionReceipt{Fence: request.Fence, MutationID: request.MutationID, AppliedWorkspaceCursor: s.cursor, HostCursor: s.hostCursor}
	s.consume(refs)
	s.remember(request.MutationID, digest, value)
	return value, nil
}

func (s *State) Apply(request execenv.ProjectionApply, commit func(Plan) error) (execenv.ProjectionReceipt, error) {
	if err := execenv.ValidateProjectionApply(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	raw, _ := json.Marshal(request)
	digest := sha256.Sum256(append([]byte("apply:"), raw...))
	if old, ok, err := s.replay(request.MutationID, digest); ok {
		return old, err
	}
	if len(s.receipts) >= retainedReceipts {
		return execenv.ProjectionReceipt{}, execenv.ErrTooLarge
	}
	if !s.initialized || request.FromWorkspaceCursor != s.cursor || request.ExpectedHostCursor != s.hostCursor {
		return execenv.ProjectionReceipt{}, execenv.ErrConflict
	}
	next := maps.Clone(s.nodes)
	plan := Plan{}
	refs := make([]*execenv.ContentReference, 0, len(request.Mutations))
	for _, mutation := range request.Mutations {
		before := maps.Clone(next)
		proof, echo := s.proofs[mutation.JournalCursor]
		if echo {
			if proof.digest != normalizedMutation(mutation) {
				return execenv.ProjectionReceipt{}, execenv.ErrConflict
			}
			if mutation.Operation != execenv.ProjectionCreate && next[mutation.Path].Identity != proof.event.NodeIdentity {
				return execenv.ProjectionReceipt{}, execenv.ErrConflict
			}
		}
		if err := s.applyOne(next, mutation); err != nil {
			return execenv.ProjectionReceipt{}, err
		}
		if echo && mutation.Operation == execenv.ProjectionCreate {
			node := next[mutation.Path]
			node.Identity = proof.event.NodeIdentity
			next[mutation.Path] = node
		}
		refs = append(refs, mutation.Content)
		converted := execenv.Mutation{Path: mutation.Path, Kind: nodeKind(mutation.Kind), Expected: mutation.ExpectedContentVersion, Version: mutation.ResultingContentVersion}
		if mutation.Recursive != nil {
			converted.Recursive = *mutation.Recursive
		}
		switch mutation.Operation {
		case execenv.ProjectionCreate:
			converted.Op = execenv.OpCreate
			converted.Data = next[mutation.Path].Data
		case execenv.ProjectionWrite:
			converted.Op = execenv.OpReplace
			converted.Data = next[mutation.Path].Data
		case execenv.ProjectionMove, execenv.ProjectionRename:
			converted.Op = execenv.OpMove
			converted.From = mutation.Path
			converted.Path = mutation.DestinationPath
		case execenv.ProjectionDelete:
			converted.Op = execenv.OpDelete
		}
		plan.steps = append(plan.steps, step{before: before, after: maps.Clone(next), mutation: converted, echo: echo})
	}
	if err := validateSize(next); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if err := commit(plan); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	s.nodes = next
	s.cursor = request.ThroughWorkspaceCursor
	value := execenv.ProjectionReceipt{Fence: request.Fence, MutationID: request.MutationID, AppliedWorkspaceCursor: s.cursor, HostCursor: s.hostCursor}
	s.consume(refs)
	s.remember(request.MutationID, digest, value)
	return value, nil
}

func (s *State) applyOne(nodes map[string]Node, m execenv.ProjectionMutation) error {
	current, exists := nodes[m.Path]
	if m.Operation == execenv.ProjectionCreate {
		if exists {
			return execenv.ErrConflict
		}
		for _, node := range nodes {
			if node.EntryID == m.EntryID {
				return execenv.ErrConflict
			}
		}
		if err := requireParent(nodes, m.Path); err != nil {
			return err
		}
		node := Node{Identity: rand.Text(), EntryID: m.EntryID, Kind: m.Kind, Version: m.ResultingContentVersion}
		if m.Kind == execenv.ProjectionFile {
			body, err := s.content(m.Content)
			if err != nil {
				return err
			}
			node.Data = body
		}
		nodes[m.Path] = node
		return nil
	}
	if !exists || current.EntryID != m.EntryID || current.Kind != m.Kind {
		return execenv.ErrConflict
	}
	if current.Kind == execenv.ProjectionFile && current.Version != m.ExpectedContentVersion {
		return execenv.ErrConflict
	}
	switch m.Operation {
	case execenv.ProjectionWrite:
		body, err := s.content(m.Content)
		if err != nil {
			return err
		}
		current.Data = body
		current.Version = m.ResultingContentVersion
		nodes[m.Path] = current
	case execenv.ProjectionMove, execenv.ProjectionRename:
		if strings.HasPrefix(m.DestinationPath, m.Path+"/") {
			return execenv.ErrConflict
		}
		if err := requireParent(nodes, m.DestinationPath); err != nil {
			return err
		}
		moved := make(map[string]Node)
		for name, node := range nodes {
			if name == m.Path || strings.HasPrefix(name, m.Path+"/") {
				destination := m.DestinationPath + strings.TrimPrefix(name, m.Path)
				if execenv.ValidatePath(destination) != nil {
					return execenv.ErrInvalid
				}
				if _, ok := nodes[destination]; ok {
					return execenv.ErrConflict
				}
				moved[destination] = node
			}
		}
		for name := range nodes {
			if name == m.Path || strings.HasPrefix(name, m.Path+"/") {
				delete(nodes, name)
			}
		}
		maps.Copy(nodes, moved)
	case execenv.ProjectionDelete:
		for name := range nodes {
			if strings.HasPrefix(name, m.Path+"/") {
				if m.Recursive == nil || !*m.Recursive {
					return execenv.ErrConflict
				}
				delete(nodes, name)
			}
		}
		delete(nodes, m.Path)
	default:
		return execenv.ErrInvalid
	}
	return nil
}

func requireParent(nodes map[string]Node, name string) error {
	parent := path.Dir(name)
	if parent == "." {
		return nil
	}
	node, ok := nodes[parent]
	if !ok || node.Kind != execenv.ProjectionDirectory {
		return execenv.ErrConflict
	}
	return nil
}
func validateSize(nodes map[string]Node) error {
	if len(nodes) > execenv.MaxTreeEntries {
		return execenv.ErrTooLarge
	}
	var total int64
	for _, node := range nodes {
		total += int64(len(node.Data))
	}
	if total > execenv.MaxTreeBytes {
		return execenv.ErrTooLarge
	}
	return nil
}
func nodeKind(kind execenv.ProjectionKind) execenv.NodeKind {
	if kind == execenv.ProjectionDirectory {
		return execenv.KindDirectory
	}
	return execenv.KindFile
}

// GuestChanged fences projection until the guest observation has been reconciled.
// Losing that observation must never turn its bytes into an untracked overwrite.
func (s *State) GuestChanged() error {
	if s.hostCursor == execenv.MaxProjectionCursor {
		return execenv.ErrLagged
	}
	s.hostCursor++
	return nil
}

// Nodes returns a detached identity/version baseline for observation binding.
func (s *State) Nodes() map[string]Node {
	result := maps.Clone(s.nodes)
	for name, node := range result {
		node.Data = bytes.Clone(node.Data)
		result[name] = node
	}
	return result
}
