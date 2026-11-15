// Package wire converts between the execenv contract and its protobuf
// schema, and between execenv errors and Connect errors. Both the remote
// link and the guest link use it.
package wire

import (
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

// Conversions between the contract and its schema. An enum value outside
// the schema converts to its zero value, which every validator refuses.

func NetworkTo(n execenv.Network) execenvv1.Network {
	switch n {
	case execenv.NetworkNone:
		return execenvv1.Network_NETWORK_NONE
	case execenv.NetworkAllowlist:
		return execenvv1.Network_NETWORK_ALLOWLIST
	default:
		return execenvv1.Network_NETWORK_UNSPECIFIED
	}
}

func NetworkFrom(n execenvv1.Network) (execenv.Network, bool) {
	switch n {
	case execenvv1.Network_NETWORK_NONE:
		return execenv.NetworkNone, true
	case execenvv1.Network_NETWORK_ALLOWLIST:
		return execenv.NetworkAllowlist, true
	default:
		return 0, false
	}
}

func IsolationTo(i execenv.Isolation) execenvv1.Isolation {
	switch i {
	case execenv.IsolationNone:
		return execenvv1.Isolation_ISOLATION_NONE
	case execenv.IsolationVM:
		return execenvv1.Isolation_ISOLATION_VM
	default:
		return execenvv1.Isolation_ISOLATION_UNSPECIFIED
	}
}

func IsolationFrom(i execenvv1.Isolation) execenv.Isolation {
	switch i {
	case execenvv1.Isolation_ISOLATION_NONE:
		return execenv.IsolationNone
	case execenvv1.Isolation_ISOLATION_VM:
		return execenv.IsolationVM
	default:
		return ""
	}
}

func ReportTo(r execenv.Report) *execenvv1.Report {
	out := &execenvv1.Report{
		Usable:      r.Usable,
		Reason:      r.Reason,
		Grants:      int64(r.Grants),
		Lost:        int64(r.Lost),
		Release:     r.Release,
		Isolation:   IsolationTo(r.Isolation),
		CpuMillis:   capacityTo(r.CPUMillis),
		MemoryBytes: capacityTo(r.MemoryBytes),
		DiskBytes:   capacityTo(r.DiskBytes),
		Defaults:    ResourcesTo(r.Defaults),
		GrantMax:    ResourcesTo(r.GrantMax),
	}
	for _, image := range r.Images {
		out.Images = append(out.Images, string(image))
	}
	for _, n := range r.Networks {
		out.Networks = append(out.Networks, NetworkTo(n))
	}
	for _, tpl := range r.Templates {
		out.Templates = append(out.Templates, &execenvv1.TemplateStatus{Image: string(tpl.Image), Shape: tpl.Shape, Network: tpl.Network, Ready: tpl.Ready, Reason: tpl.Reason})
	}
	return out
}

func ReportFrom(r *execenvv1.Report) execenv.Report {
	out := execenv.Report{
		Usable:      r.GetUsable(),
		Reason:      r.GetReason(),
		Grants:      int(r.GetGrants()),
		Lost:        int(r.GetLost()),
		Release:     r.GetRelease(),
		Isolation:   IsolationFrom(r.GetIsolation()),
		CPUMillis:   capacityFrom(r.GetCpuMillis()),
		MemoryBytes: capacityFrom(r.GetMemoryBytes()),
		DiskBytes:   capacityFrom(r.GetDiskBytes()),
		Defaults:    ResourcesFrom(r.GetDefaults()),
		GrantMax:    ResourcesFrom(r.GetGrantMax()),
	}
	for _, image := range r.GetImages() {
		out.Images = append(out.Images, execenv.Image(image))
	}
	for _, n := range r.GetNetworks() {
		if network, ok := NetworkFrom(n); ok {
			out.Networks = append(out.Networks, network)
		}
	}
	for _, tpl := range r.GetTemplates() {
		out.Templates = append(out.Templates, execenv.TemplateStatus{Image: execenv.Image(tpl.GetImage()), Shape: tpl.GetShape(), Network: tpl.GetNetwork(), Ready: tpl.GetReady(), Reason: tpl.GetReason()})
	}
	return out
}

func capacityTo(c execenv.Capacity) *execenvv1.Capacity {
	return &execenvv1.Capacity{Total: c.Total, Reserved: c.Reserved, Free: c.Free}
}

func capacityFrom(c *execenvv1.Capacity) execenv.Capacity {
	return execenv.Capacity{Total: c.GetTotal(), Reserved: c.GetReserved(), Free: c.GetFree()}
}

func ResourcesTo(r execenv.Resources) *execenvv1.Resources {
	return &execenvv1.Resources{CpuMillis: r.CPUMillis, MemoryBytes: r.MemoryBytes, DiskBytes: r.DiskBytes}
}

func ResourcesFrom(r *execenvv1.Resources) execenv.Resources {
	return execenv.Resources{CPUMillis: r.GetCpuMillis(), MemoryBytes: r.GetMemoryBytes(), DiskBytes: r.GetDiskBytes()}
}

func bucketTo(b execenv.TokenBucket) *execenvv1.TokenBucket {
	return &execenvv1.TokenBucket{Rate: b.Rate, Burst: b.Burst}
}

func bucketFrom(b *execenvv1.TokenBucket) execenv.TokenBucket {
	return execenv.TokenBucket{Rate: b.GetRate(), Burst: b.GetBurst()}
}

func LimitsTo(l execenv.Limits) *execenvv1.Limits {
	return &execenvv1.Limits{DiskOps: bucketTo(l.DiskOps), DiskBytes: bucketTo(l.DiskBytes), NetworkBytes: bucketTo(l.NetworkBytes)}
}

func LimitsFrom(l *execenvv1.Limits) execenv.Limits {
	return execenv.Limits{DiskOps: bucketFrom(l.GetDiskOps()), DiskBytes: bucketFrom(l.GetDiskBytes()), NetworkBytes: bucketFrom(l.GetNetworkBytes())}
}

var idleActions = map[execenv.IdleAction]execenvv1.IdleAction{
	execenv.IdleFreeze:  execenvv1.IdleAction_IDLE_ACTION_FREEZE,
	execenv.IdleRevoke:  execenvv1.IdleAction_IDLE_ACTION_REVOKE,
	execenv.IdleSuspend: execenvv1.IdleAction_IDLE_ACTION_SUSPEND,
}

func SpecTo(s execenv.Spec) *execenvv1.Spec {
	out := &execenvv1.Spec{
		Id:         string(s.ID),
		Image:      string(s.Image),
		Network:    NetworkTo(s.Network),
		Resources:  ResourcesTo(s.Resources),
		IdleAction: idleActions[s.IdleAction],
		Labels:     s.Labels,
		Limits:     LimitsTo(s.Limits),
		Seed:       s.Seed,
	}
	if s.WallTimeout != 0 {
		out.WallTimeout = durationpb.New(s.WallTimeout)
	}
	if s.IdleTimeout != 0 {
		out.IdleTimeout = durationpb.New(s.IdleTimeout)
	}
	return out
}

// SpecFrom reports false for an enum outside the schema.
func SpecFrom(s *execenvv1.Spec) (execenv.Spec, bool) {
	network, ok := NetworkFrom(s.GetNetwork())
	out := execenv.Spec{
		ID:        execenv.ID(s.GetId()),
		Image:     execenv.Image(s.GetImage()),
		Network:   network,
		Resources: ResourcesFrom(s.GetResources()),
		Labels:    s.GetLabels(),
		Limits:    LimitsFrom(s.GetLimits()),
		Seed:      s.GetSeed(),
	}
	if len(out.Labels) == 0 {
		out.Labels = nil
	}
	if s.WallTimeout != nil {
		out.WallTimeout = s.GetWallTimeout().AsDuration()
	}
	if s.IdleTimeout != nil {
		out.IdleTimeout = s.GetIdleTimeout().AsDuration()
	}
	if action := s.GetIdleAction(); action != execenvv1.IdleAction_IDLE_ACTION_UNSPECIFIED {
		found := false
		for k, v := range idleActions {
			if v == action {
				out.IdleAction, found = k, true
			}
		}
		ok = ok && found
	}
	return out, ok
}

func FenceTo(f execenv.ProjectionFence) *execenvv1.Fence {
	return &execenvv1.Fence{GrantId: string(f.ExecutionGrantID), Epoch: f.EnvironmentEpoch, Revision: f.FenceRevision}
}

func FenceFrom(f *execenvv1.Fence) execenv.ProjectionFence {
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

func ControlStateTo(s execenv.ControlState) execenvv1.ControlState { return controlStates[s] }

func ControlStateFrom(s execenvv1.ControlState) execenv.ControlState {
	for k, v := range controlStates {
		if v == s {
			return k
		}
	}
	return ""
}

func ReceiptTo(r execenv.ControlReceipt) *execenvv1.ControlReceipt {
	return &execenvv1.ControlReceipt{Fence: FenceTo(r.Fence), State: ControlStateTo(r.State), Confirmed: r.Confirmed}
}

func ReceiptFrom(r *execenvv1.ControlReceipt) execenv.ControlReceipt {
	return execenv.ControlReceipt{Fence: FenceFrom(r.GetFence()), State: ControlStateFrom(r.GetState()), Confirmed: r.GetConfirmed()}
}

func ContentTo(c *execenv.ContentReference) *execenvv1.ContentReference {
	if c == nil {
		return nil
	}
	return &execenvv1.ContentReference{Size: c.Size, Sha256: c.Digest, TransferId: c.TransferID}
}

func ContentFrom(c *execenvv1.ContentReference) *execenv.ContentReference {
	if c == nil {
		return nil
	}
	return &execenv.ContentReference{Size: c.GetSize(), Digest: c.GetSha256(), TransferID: c.GetTransferId()}
}

var projectionKinds = map[execenv.ProjectionKind]execenvv1.ProjectionKind{
	execenv.ProjectionFile:      execenvv1.ProjectionKind_PROJECTION_KIND_FILE,
	execenv.ProjectionDirectory: execenvv1.ProjectionKind_PROJECTION_KIND_DIRECTORY,
}

func KindTo(k execenv.ProjectionKind) execenvv1.ProjectionKind { return projectionKinds[k] }

func KindFrom(k execenvv1.ProjectionKind) execenv.ProjectionKind {
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

func OperationFrom(o execenvv1.ProjectionOperation) execenv.ProjectionOperation {
	for key, v := range projectionOperations {
		if v == o {
			return key
		}
	}
	return ""
}

func MutationTo(m execenv.ProjectionMutation) *execenvv1.ProjectionMutation {
	return &execenvv1.ProjectionMutation{
		JournalCursor:           m.JournalCursor,
		EntryId:                 m.EntryID,
		Operation:               projectionOperations[m.Operation],
		Kind:                    KindTo(m.Kind),
		Path:                    m.Path,
		DestinationPath:         m.DestinationPath,
		ExpectedContentVersion:  string(m.ExpectedContentVersion),
		ResultingContentVersion: string(m.ResultingContentVersion),
		Recursive:               m.Recursive,
		Content:                 ContentTo(m.Content),
	}
}

func MutationFrom(m *execenvv1.ProjectionMutation) execenv.ProjectionMutation {
	return execenv.ProjectionMutation{
		JournalCursor:           m.GetJournalCursor(),
		EntryID:                 m.GetEntryId(),
		Operation:               OperationFrom(m.GetOperation()),
		Kind:                    KindFrom(m.GetKind()),
		Path:                    m.GetPath(),
		DestinationPath:         m.GetDestinationPath(),
		ExpectedContentVersion:  execenv.Version(m.GetExpectedContentVersion()),
		ResultingContentVersion: execenv.Version(m.GetResultingContentVersion()),
		Recursive:               m.Recursive,
		Content:                 ContentFrom(m.GetContent()),
	}
}

func MutationsTo(ms []execenv.ProjectionMutation) []*execenvv1.ProjectionMutation {
	out := make([]*execenvv1.ProjectionMutation, 0, len(ms))
	for _, m := range ms {
		out = append(out, MutationTo(m))
	}
	return out
}

func MutationsFrom(ms []*execenvv1.ProjectionMutation) []execenv.ProjectionMutation {
	out := make([]execenv.ProjectionMutation, 0, len(ms))
	for _, m := range ms {
		out = append(out, MutationFrom(m))
	}
	return out
}

func InitializationTo(r execenv.ProjectionInitialization) *execenvv1.InitializeProjectionRequest {
	out := &execenvv1.InitializeProjectionRequest{
		Fence:           FenceTo(r.Fence),
		MutationId:      r.MutationID,
		WorkspaceCursor: r.WorkspaceCursor,
	}
	for _, e := range r.Entries {
		out.Entries = append(out.Entries, &execenvv1.ProjectionEntry{
			EntryId:        e.EntryID,
			Kind:           KindTo(e.Kind),
			Path:           e.Path,
			ContentVersion: string(e.ContentVersion),
			Content:        ContentTo(e.Content),
		})
	}
	if r.Ignore != nil {
		out.Ignore = &execenvv1.IgnoreRules{File: r.Ignore.File, Fallback: r.Ignore.Fallback}
	}
	return out
}

func InitializationFrom(r *execenvv1.InitializeProjectionRequest) execenv.ProjectionInitialization {
	out := execenv.ProjectionInitialization{
		Fence:           FenceFrom(r.GetFence()),
		MutationID:      r.GetMutationId(),
		WorkspaceCursor: r.GetWorkspaceCursor(),
		Entries:         make([]execenv.ProjectionEntry, 0, len(r.GetEntries())),
	}
	for _, e := range r.GetEntries() {
		out.Entries = append(out.Entries, execenv.ProjectionEntry{
			EntryID:        e.GetEntryId(),
			Kind:           KindFrom(e.GetKind()),
			Path:           e.GetPath(),
			ContentVersion: execenv.Version(e.GetContentVersion()),
			Content:        ContentFrom(e.GetContent()),
		})
	}
	if ignore := r.GetIgnore(); ignore != nil {
		out.Ignore = &execenv.IgnoreRules{File: ignore.GetFile(), Fallback: ignore.GetFallback()}
	}
	return out
}

func ApplyTo(r execenv.ProjectionApply) *execenvv1.ApplyProjectionRequest {
	return &execenvv1.ApplyProjectionRequest{
		Fence:                  FenceTo(r.Fence),
		MutationId:             r.MutationID,
		FromWorkspaceCursor:    r.FromWorkspaceCursor,
		ThroughWorkspaceCursor: r.ThroughWorkspaceCursor,
		ExpectedHostCursor:     r.ExpectedHostCursor,
		Mutations:              MutationsTo(r.Mutations),
	}
}

func ApplyFrom(r *execenvv1.ApplyProjectionRequest) execenv.ProjectionApply {
	return execenv.ProjectionApply{
		Fence:                  FenceFrom(r.GetFence()),
		MutationID:             r.GetMutationId(),
		FromWorkspaceCursor:    r.GetFromWorkspaceCursor(),
		ThroughWorkspaceCursor: r.GetThroughWorkspaceCursor(),
		ExpectedHostCursor:     r.GetExpectedHostCursor(),
		Mutations:              MutationsFrom(r.GetMutations()),
	}
}

func ProjectionReceiptTo(r execenv.ProjectionReceipt) *execenvv1.ProjectionReceipt {
	return &execenvv1.ProjectionReceipt{
		Fence:                  FenceTo(r.Fence),
		MutationId:             r.MutationID,
		AppliedWorkspaceCursor: r.AppliedWorkspaceCursor,
		HostCursor:             r.HostCursor,
	}
}

func ProjectionReceiptFrom(r *execenvv1.ProjectionReceipt) execenv.ProjectionReceipt {
	return execenv.ProjectionReceipt{
		Fence:                  FenceFrom(r.GetFence()),
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

func ObservationOperationFrom(o execenvv1.ObservationOperation) execenv.ObservationOperation {
	for key, v := range observationOperations {
		if v == o {
			return key
		}
	}
	return ""
}

func ObservationPageTo(p execenv.ObservationPage) *execenvv1.ObservationPage {
	out := &execenvv1.ObservationPage{HostCursor: p.HostCursor, AcknowledgedHostCursor: p.AcknowledgedHostCursor}
	for _, o := range p.Observations {
		out.Observations = append(out.Observations, &execenvv1.Observation{
			Fence:                      FenceTo(o.Fence),
			HostSequence:               o.HostSequence,
			BasedOnWorkspaceCursor:     o.BasedOnWorkspaceCursor,
			Operation:                  observationOperations[o.Operation],
			NodeIdentity:               o.NodeIdentity,
			Kind:                       KindTo(o.Kind),
			Path:                       o.Path,
			DestinationPath:            o.DestinationPath,
			ExpectedContentVersion:     string(o.ExpectedContentVersion),
			Recursive:                  o.Recursive,
			Content:                    ContentTo(o.Content),
			OriginProjectionMutationId: o.OriginProjectionMutationID,
		})
	}
	return out
}

func ObservationPageFrom(p *execenvv1.ObservationPage) execenv.ObservationPage {
	out := execenv.ObservationPage{
		HostCursor:             p.GetHostCursor(),
		AcknowledgedHostCursor: p.GetAcknowledgedHostCursor(),
		Observations:           make([]execenv.ExecutionObservation, 0, len(p.GetObservations())),
	}
	for _, o := range p.GetObservations() {
		out.Observations = append(out.Observations, execenv.ExecutionObservation{
			Fence:                      FenceFrom(o.GetFence()),
			HostSequence:               o.GetHostSequence(),
			BasedOnWorkspaceCursor:     o.GetBasedOnWorkspaceCursor(),
			Operation:                  ObservationOperationFrom(o.GetOperation()),
			NodeIdentity:               o.GetNodeIdentity(),
			Kind:                       KindFrom(o.GetKind()),
			Path:                       o.GetPath(),
			DestinationPath:            o.GetDestinationPath(),
			ExpectedContentVersion:     execenv.Version(o.GetExpectedContentVersion()),
			Recursive:                  o.Recursive,
			Content:                    ContentFrom(o.GetContent()),
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

func ProcessKindFrom(k execenvv1.ProcessKind) execenv.ProcessKind {
	for key, v := range processKinds {
		if v == k {
			return key
		}
	}
	return ""
}

func ProcessPageTo(p execenv.ProcessPage) *execenvv1.ProcessPage {
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
			Tag:       e.Tag,
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

func ProcessPageFrom(p *execenvv1.ProcessPage) execenv.ProcessPage {
	out := execenv.ProcessPage{Next: p.GetNext(), Events: make([]execenv.ProcessEvent, 0, len(p.GetEvents()))}
	for _, e := range p.GetEvents() {
		event := execenv.ProcessEvent{
			Sequence:  e.GetSequence(),
			Kind:      ProcessKindFrom(e.GetKind()),
			PID:       int(e.GetPid()),
			ParentPID: int(e.GetParentPid()),
			Argv:      e.GetArgv(),
			Truncated: e.GetTruncated(),
			Unread:    e.GetUnread(),
			Dir:       e.GetDir(),
			Tag:       e.GetTag(),
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

func ScreenTo(s execenv.Screen) *execenvv1.Screen {
	return &execenvv1.Screen{
		Data: s.Data, Offset: s.Offset, Window: WindowTo(s.Window), AltScreen: s.AltScreen,
		CursorRow: int32(s.CursorRow), CursorCol: int32(s.CursorCol), CursorHidden: s.CursorHidden, Text: s.Text,
	}
}

func ScreenFrom(s *execenvv1.Screen) execenv.Screen {
	win, _ := WindowFrom(s.GetWindow())
	return execenv.Screen{
		Data: s.GetData(), Offset: s.GetOffset(), Window: win, AltScreen: s.GetAltScreen(),
		CursorRow: int(s.GetCursorRow()), CursorCol: int(s.GetCursorCol()), CursorHidden: s.GetCursorHidden(), Text: s.GetText(),
	}
}

func WindowTo(w execenv.Window) *execenvv1.Window {
	return &execenvv1.Window{Cols: uint32(w.Cols), Rows: uint32(w.Rows)}
}

func WindowFrom(w *execenvv1.Window) (execenv.Window, bool) {
	if w.GetCols() > 0xffff || w.GetRows() > 0xffff {
		return execenv.Window{}, false
	}
	return execenv.Window{Cols: uint16(w.GetCols()), Rows: uint16(w.GetRows())}, true
}
