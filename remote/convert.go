package remote

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

// Conversions between the contract and its schema. An enum value outside
// the schema converts to its zero value, which every validator refuses.

func networkTo(n execenv.Network) execenvv1.Network {
	switch n {
	case execenv.NetworkNone:
		return execenvv1.Network_NETWORK_NONE
	case execenv.NetworkAllowlist:
		return execenvv1.Network_NETWORK_ALLOWLIST
	default:
		return execenvv1.Network_NETWORK_UNSPECIFIED
	}
}

func networkFrom(n execenvv1.Network) (execenv.Network, bool) {
	switch n {
	case execenvv1.Network_NETWORK_NONE:
		return execenv.NetworkNone, true
	case execenvv1.Network_NETWORK_ALLOWLIST:
		return execenv.NetworkAllowlist, true
	default:
		return 0, false
	}
}

func isolationTo(i execenv.Isolation) execenvv1.Isolation {
	switch i {
	case execenv.IsolationNone:
		return execenvv1.Isolation_ISOLATION_NONE
	case execenv.IsolationVM:
		return execenvv1.Isolation_ISOLATION_VM
	default:
		return execenvv1.Isolation_ISOLATION_UNSPECIFIED
	}
}

func isolationFrom(i execenvv1.Isolation) execenv.Isolation {
	switch i {
	case execenvv1.Isolation_ISOLATION_NONE:
		return execenv.IsolationNone
	case execenvv1.Isolation_ISOLATION_VM:
		return execenv.IsolationVM
	default:
		return ""
	}
}

func reportTo(r execenv.Report) *execenvv1.Report {
	out := &execenvv1.Report{
		Usable:    r.Usable,
		Reason:    r.Reason,
		Slots:     int64(r.Slots),
		Grants:    int64(r.Grants),
		Lost:      int64(r.Lost),
		Release:   r.Release,
		Isolation: isolationTo(r.Isolation),
	}
	for _, image := range r.Images {
		out.Images = append(out.Images, string(image))
	}
	for _, n := range r.Networks {
		out.Networks = append(out.Networks, networkTo(n))
	}
	return out
}

func reportFrom(r *execenvv1.Report) execenv.Report {
	out := execenv.Report{
		Usable:    r.GetUsable(),
		Reason:    r.GetReason(),
		Slots:     int(r.GetSlots()),
		Grants:    int(r.GetGrants()),
		Lost:      int(r.GetLost()),
		Release:   r.GetRelease(),
		Isolation: isolationFrom(r.GetIsolation()),
	}
	for _, image := range r.GetImages() {
		out.Images = append(out.Images, execenv.Image(image))
	}
	for _, n := range r.GetNetworks() {
		if network, ok := networkFrom(n); ok {
			out.Networks = append(out.Networks, network)
		}
	}
	return out
}

func specTo(s execenv.Spec) *execenvv1.Spec {
	return &execenvv1.Spec{Id: string(s.ID), Image: string(s.Image), Network: networkTo(s.Network)}
}

func specFrom(s *execenvv1.Spec) (execenv.Spec, bool) {
	network, ok := networkFrom(s.GetNetwork())
	return execenv.Spec{ID: execenv.ID(s.GetId()), Image: execenv.Image(s.GetImage()), Network: network}, ok
}

func fenceTo(f execenv.ProjectionFence) *execenvv1.Fence {
	return &execenvv1.Fence{GrantId: string(f.ExecutionGrantID), Epoch: f.EnvironmentEpoch, Revision: f.FenceRevision}
}

func fenceFrom(f *execenvv1.Fence) execenv.ProjectionFence {
	return execenv.ProjectionFence{
		ExecutionGrantID: execenv.ID(f.GetGrantId()),
		EnvironmentEpoch: f.GetEpoch(),
		FenceRevision:    f.GetRevision(),
	}
}

var controlStates = map[execenv.ControlState]execenvv1.ControlState{
	execenv.ControlRunning: execenvv1.ControlState_CONTROL_STATE_RUNNING,
	execenv.ControlFrozen:  execenvv1.ControlState_CONTROL_STATE_FROZEN,
	execenv.ControlRevoked: execenvv1.ControlState_CONTROL_STATE_REVOKED,
}

func controlStateTo(s execenv.ControlState) execenvv1.ControlState { return controlStates[s] }

func controlStateFrom(s execenvv1.ControlState) execenv.ControlState {
	for k, v := range controlStates {
		if v == s {
			return k
		}
	}
	return ""
}

func receiptTo(r execenv.ControlReceipt) *execenvv1.ControlReceipt {
	return &execenvv1.ControlReceipt{Fence: fenceTo(r.Fence), State: controlStateTo(r.State), Confirmed: r.Confirmed}
}

func receiptFrom(r *execenvv1.ControlReceipt) execenv.ControlReceipt {
	return execenv.ControlReceipt{Fence: fenceFrom(r.GetFence()), State: controlStateFrom(r.GetState()), Confirmed: r.GetConfirmed()}
}

func contentTo(c *execenv.ContentReference) *execenvv1.ContentReference {
	if c == nil {
		return nil
	}
	return &execenvv1.ContentReference{Size: c.Size, Sha256: c.Digest, TransferId: c.TransferID}
}

func contentFrom(c *execenvv1.ContentReference) *execenv.ContentReference {
	if c == nil {
		return nil
	}
	return &execenv.ContentReference{Size: c.GetSize(), Digest: c.GetSha256(), TransferID: c.GetTransferId()}
}

var projectionKinds = map[execenv.ProjectionKind]execenvv1.ProjectionKind{
	execenv.ProjectionFile:      execenvv1.ProjectionKind_PROJECTION_KIND_FILE,
	execenv.ProjectionDirectory: execenvv1.ProjectionKind_PROJECTION_KIND_DIRECTORY,
}

func kindTo(k execenv.ProjectionKind) execenvv1.ProjectionKind { return projectionKinds[k] }

func kindFrom(k execenvv1.ProjectionKind) execenv.ProjectionKind {
	for key, v := range projectionKinds {
		if v == k {
			return key
		}
	}
	return ""
}

var projectionOperations = map[execenv.ProjectionOperation]execenvv1.ProjectionOperation{
	execenv.ProjectionCreate: execenvv1.ProjectionOperation_PROJECTION_OPERATION_CREATE,
	execenv.ProjectionWrite:  execenvv1.ProjectionOperation_PROJECTION_OPERATION_WRITE,
	execenv.ProjectionRename: execenvv1.ProjectionOperation_PROJECTION_OPERATION_RENAME,
	execenv.ProjectionMove:   execenvv1.ProjectionOperation_PROJECTION_OPERATION_MOVE,
	execenv.ProjectionDelete: execenvv1.ProjectionOperation_PROJECTION_OPERATION_DELETE,
}

func operationFrom(o execenvv1.ProjectionOperation) execenv.ProjectionOperation {
	for key, v := range projectionOperations {
		if v == o {
			return key
		}
	}
	return ""
}

func mutationTo(m execenv.ProjectionMutation) *execenvv1.ProjectionMutation {
	return &execenvv1.ProjectionMutation{
		JournalCursor:           m.JournalCursor,
		EntryId:                 m.EntryID,
		Operation:               projectionOperations[m.Operation],
		Kind:                    kindTo(m.Kind),
		Path:                    m.Path,
		DestinationPath:         m.DestinationPath,
		ExpectedContentVersion:  string(m.ExpectedContentVersion),
		ResultingContentVersion: string(m.ResultingContentVersion),
		Recursive:               m.Recursive,
		Content:                 contentTo(m.Content),
	}
}

func mutationFrom(m *execenvv1.ProjectionMutation) execenv.ProjectionMutation {
	return execenv.ProjectionMutation{
		JournalCursor:           m.GetJournalCursor(),
		EntryID:                 m.GetEntryId(),
		Operation:               operationFrom(m.GetOperation()),
		Kind:                    kindFrom(m.GetKind()),
		Path:                    m.GetPath(),
		DestinationPath:         m.GetDestinationPath(),
		ExpectedContentVersion:  execenv.Version(m.GetExpectedContentVersion()),
		ResultingContentVersion: execenv.Version(m.GetResultingContentVersion()),
		Recursive:               m.Recursive,
		Content:                 contentFrom(m.GetContent()),
	}
}

func mutationsTo(ms []execenv.ProjectionMutation) []*execenvv1.ProjectionMutation {
	out := make([]*execenvv1.ProjectionMutation, 0, len(ms))
	for _, m := range ms {
		out = append(out, mutationTo(m))
	}
	return out
}

func mutationsFrom(ms []*execenvv1.ProjectionMutation) []execenv.ProjectionMutation {
	out := make([]execenv.ProjectionMutation, 0, len(ms))
	for _, m := range ms {
		out = append(out, mutationFrom(m))
	}
	return out
}

func initializationTo(r execenv.ProjectionInitialization) *execenvv1.InitializeProjectionRequest {
	out := &execenvv1.InitializeProjectionRequest{
		Fence:           fenceTo(r.Fence),
		MutationId:      r.MutationID,
		WorkspaceCursor: r.WorkspaceCursor,
	}
	for _, e := range r.Entries {
		out.Entries = append(out.Entries, &execenvv1.ProjectionEntry{
			EntryId:        e.EntryID,
			Kind:           kindTo(e.Kind),
			Path:           e.Path,
			ContentVersion: string(e.ContentVersion),
			Content:        contentTo(e.Content),
		})
	}
	if r.Ignore != nil {
		out.Ignore = &execenvv1.IgnoreRules{File: r.Ignore.File, Fallback: r.Ignore.Fallback}
	}
	return out
}

func initializationFrom(r *execenvv1.InitializeProjectionRequest) execenv.ProjectionInitialization {
	out := execenv.ProjectionInitialization{
		Fence:           fenceFrom(r.GetFence()),
		MutationID:      r.GetMutationId(),
		WorkspaceCursor: r.GetWorkspaceCursor(),
		Entries:         make([]execenv.ProjectionEntry, 0, len(r.GetEntries())),
	}
	for _, e := range r.GetEntries() {
		out.Entries = append(out.Entries, execenv.ProjectionEntry{
			EntryID:        e.GetEntryId(),
			Kind:           kindFrom(e.GetKind()),
			Path:           e.GetPath(),
			ContentVersion: execenv.Version(e.GetContentVersion()),
			Content:        contentFrom(e.GetContent()),
		})
	}
	if ignore := r.GetIgnore(); ignore != nil {
		out.Ignore = &execenv.IgnoreRules{File: ignore.GetFile(), Fallback: ignore.GetFallback()}
	}
	return out
}

func applyTo(r execenv.ProjectionApply) *execenvv1.ApplyProjectionRequest {
	return &execenvv1.ApplyProjectionRequest{
		Fence:                  fenceTo(r.Fence),
		MutationId:             r.MutationID,
		FromWorkspaceCursor:    r.FromWorkspaceCursor,
		ThroughWorkspaceCursor: r.ThroughWorkspaceCursor,
		ExpectedHostCursor:     r.ExpectedHostCursor,
		Mutations:              mutationsTo(r.Mutations),
	}
}

func applyFrom(r *execenvv1.ApplyProjectionRequest) execenv.ProjectionApply {
	return execenv.ProjectionApply{
		Fence:                  fenceFrom(r.GetFence()),
		MutationID:             r.GetMutationId(),
		FromWorkspaceCursor:    r.GetFromWorkspaceCursor(),
		ThroughWorkspaceCursor: r.GetThroughWorkspaceCursor(),
		ExpectedHostCursor:     r.GetExpectedHostCursor(),
		Mutations:              mutationsFrom(r.GetMutations()),
	}
}

func projectionReceiptTo(r execenv.ProjectionReceipt) *execenvv1.ProjectionReceipt {
	return &execenvv1.ProjectionReceipt{
		Fence:                  fenceTo(r.Fence),
		MutationId:             r.MutationID,
		AppliedWorkspaceCursor: r.AppliedWorkspaceCursor,
		HostCursor:             r.HostCursor,
	}
}

func projectionReceiptFrom(r *execenvv1.ProjectionReceipt) execenv.ProjectionReceipt {
	return execenv.ProjectionReceipt{
		Fence:                  fenceFrom(r.GetFence()),
		MutationID:             r.GetMutationId(),
		AppliedWorkspaceCursor: r.GetAppliedWorkspaceCursor(),
		HostCursor:             r.GetHostCursor(),
	}
}

var observationOperations = map[execenv.ObservationOperation]execenvv1.ObservationOperation{
	execenv.ObservationCreateFile:      execenvv1.ObservationOperation_OBSERVATION_OPERATION_CREATE_FILE,
	execenv.ObservationCreateDirectory: execenvv1.ObservationOperation_OBSERVATION_OPERATION_CREATE_DIRECTORY,
	execenv.ObservationWriteFile:       execenvv1.ObservationOperation_OBSERVATION_OPERATION_WRITE_FILE,
	execenv.ObservationMove:            execenvv1.ObservationOperation_OBSERVATION_OPERATION_MOVE,
	execenv.ObservationDelete:          execenvv1.ObservationOperation_OBSERVATION_OPERATION_DELETE,
}

func observationOperationFrom(o execenvv1.ObservationOperation) execenv.ObservationOperation {
	for key, v := range observationOperations {
		if v == o {
			return key
		}
	}
	return ""
}

func observationPageTo(p execenv.ObservationPage) *execenvv1.ObservationPage {
	out := &execenvv1.ObservationPage{HostCursor: p.HostCursor, AcknowledgedHostCursor: p.AcknowledgedHostCursor}
	for _, o := range p.Observations {
		out.Observations = append(out.Observations, &execenvv1.Observation{
			Fence:                      fenceTo(o.Fence),
			HostSequence:               o.HostSequence,
			BasedOnWorkspaceCursor:     o.BasedOnWorkspaceCursor,
			Operation:                  observationOperations[o.Operation],
			NodeIdentity:               o.NodeIdentity,
			Kind:                       kindTo(o.Kind),
			Path:                       o.Path,
			DestinationPath:            o.DestinationPath,
			ExpectedContentVersion:     string(o.ExpectedContentVersion),
			Recursive:                  o.Recursive,
			Content:                    contentTo(o.Content),
			OriginProjectionMutationId: o.OriginProjectionMutationID,
		})
	}
	return out
}

func observationPageFrom(p *execenvv1.ObservationPage) execenv.ObservationPage {
	out := execenv.ObservationPage{
		HostCursor:             p.GetHostCursor(),
		AcknowledgedHostCursor: p.GetAcknowledgedHostCursor(),
		Observations:           make([]execenv.ExecutionObservation, 0, len(p.GetObservations())),
	}
	for _, o := range p.GetObservations() {
		out.Observations = append(out.Observations, execenv.ExecutionObservation{
			Fence:                      fenceFrom(o.GetFence()),
			HostSequence:               o.GetHostSequence(),
			BasedOnWorkspaceCursor:     o.GetBasedOnWorkspaceCursor(),
			Operation:                  observationOperationFrom(o.GetOperation()),
			NodeIdentity:               o.GetNodeIdentity(),
			Kind:                       kindFrom(o.GetKind()),
			Path:                       o.GetPath(),
			DestinationPath:            o.GetDestinationPath(),
			ExpectedContentVersion:     execenv.Version(o.GetExpectedContentVersion()),
			Recursive:                  o.Recursive,
			Content:                    contentFrom(o.GetContent()),
			OriginProjectionMutationID: o.GetOriginProjectionMutationId(),
		})
	}
	return out
}

var processKinds = map[execenv.ProcessKind]execenvv1.ProcessKind{
	execenv.ProcessStarted: execenvv1.ProcessKind_PROCESS_KIND_STARTED,
	execenv.ProcessExited:  execenvv1.ProcessKind_PROCESS_KIND_EXITED,
	execenv.ProcessDropped: execenvv1.ProcessKind_PROCESS_KIND_DROPPED,
}

func processKindFrom(k execenvv1.ProcessKind) execenv.ProcessKind {
	for key, v := range processKinds {
		if v == k {
			return key
		}
	}
	return ""
}

func processPageTo(p execenv.ProcessPage) *execenvv1.ProcessPage {
	out := &execenvv1.ProcessPage{Next: p.Next}
	for _, e := range p.Events {
		event := &execenvv1.ProcessEvent{
			Sequence:  e.Sequence,
			Kind:      processKinds[e.Kind],
			Pid:       int32(e.PID),
			ParentPid: int32(e.ParentPID),
			Argv:      e.Argv,
			Truncated: e.Truncated,
			Unread:    e.Unread,
			Dir:       e.Dir,
			At:        timestamppb.New(e.At),
			Signal:    int32(e.Signal),
			Dropped:   e.Dropped,
		}
		if e.ExitCode != nil {
			code := int32(*e.ExitCode)
			event.ExitCode = &code
		}
		out.Events = append(out.Events, event)
	}
	return out
}

func processPageFrom(p *execenvv1.ProcessPage) execenv.ProcessPage {
	out := execenv.ProcessPage{Next: p.GetNext(), Events: make([]execenv.ProcessEvent, 0, len(p.GetEvents()))}
	for _, e := range p.GetEvents() {
		event := execenv.ProcessEvent{
			Sequence:  e.GetSequence(),
			Kind:      processKindFrom(e.GetKind()),
			PID:       int(e.GetPid()),
			ParentPID: int(e.GetParentPid()),
			Argv:      e.GetArgv(),
			Truncated: e.GetTruncated(),
			Unread:    e.GetUnread(),
			Dir:       e.GetDir(),
			At:        e.GetAt().AsTime(),
			Signal:    int(e.GetSignal()),
			Dropped:   e.GetDropped(),
		}
		if e.ExitCode != nil {
			code := int(e.GetExitCode())
			event.ExitCode = &code
		}
		out.Events = append(out.Events, event)
	}
	return out
}

func windowTo(w execenv.Window) *execenvv1.Window {
	return &execenvv1.Window{Cols: uint32(w.Cols), Rows: uint32(w.Rows)}
}

func windowFrom(w *execenvv1.Window) (execenv.Window, bool) {
	if w.GetCols() > 0xffff || w.GetRows() > 0xffff {
		return execenv.Window{}, false
	}
	return execenv.Window{Cols: uint16(w.GetCols()), Rows: uint16(w.GetRows())}, true
}
