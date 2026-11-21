package wire

import (
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

func ProcessSpecTo(s execenv.ProcessSpec) *execenvv1.ProcessSpec {
	out := &execenvv1.ProcessSpec{
		Tag:          s.Tag,
		Argv:         s.Argv,
		Env:          s.Env,
		Dir:          s.Dir,
		Stdin:        s.Stdin,
		HistoryBytes: s.HistoryBytes,
	}
	if s.PTY != nil {
		out.Pty = WindowTo(*s.PTY)
	}
	if s.DetachGrace > 0 {
		out.DetachGrace = durationpb.New(s.DetachGrace)
	}
	if s.Timeout > 0 {
		out.Timeout = durationpb.New(s.Timeout)
	}
	return out
}

// ProcessSpecFrom converts a spec; a terminal size out of range is false.
func ProcessSpecFrom(s *execenvv1.ProcessSpec) (execenv.ProcessSpec, bool) {
	out := execenv.ProcessSpec{
		Tag:          s.GetTag(),
		Argv:         s.GetArgv(),
		Env:          s.GetEnv(),
		Dir:          s.GetDir(),
		Stdin:        s.GetStdin(),
		HistoryBytes: s.GetHistoryBytes(),
	}
	if s.GetPty() != nil {
		w, ok := WindowFrom(s.GetPty())
		if !ok {
			return execenv.ProcessSpec{}, false
		}
		out.PTY = &w
	}
	if d := s.GetDetachGrace(); d != nil {
		out.DetachGrace = d.AsDuration()
	}
	if d := s.GetTimeout(); d != nil {
		out.Timeout = d.AsDuration()
	}
	return out, true
}

func ExitTo(e execenv.ExitStatus) *execenvv1.ExitStatus {
	return &execenvv1.ExitStatus{Code: int32(e.Code), Signal: int32(e.Signal)}
}

func ExitFrom(e *execenvv1.ExitStatus) execenv.ExitStatus {
	return execenv.ExitStatus{Code: int(e.GetCode()), Signal: int(e.GetSignal())}
}

func OffsetsTo(m map[execenv.Stream]uint64) map[string]uint64 {
	if m == nil {
		return nil
	}
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}

func OffsetsFrom(m map[string]uint64) map[execenv.Stream]uint64 {
	if m == nil {
		return nil
	}
	out := make(map[execenv.Stream]uint64, len(m))
	for k, v := range m {
		out[execenv.Stream(k)] = v
	}
	return out
}

func ProcessInfoTo(p execenv.ProcessInfo) *execenvv1.ProcessInfo {
	out := &execenvv1.ProcessInfo{
		Tag:     p.Tag,
		Pid:     int32(p.PID),
		Pty:     p.PTY,
		Running: p.Running,
		Exit:    ExitTo(p.Exit),
		Output:  OffsetsTo(p.Output),
		Input:   p.Input,
	}
	if !p.Started.IsZero() {
		out.Started = timestamppb.New(p.Started)
	}
	if !p.Ended.IsZero() {
		out.Ended = timestamppb.New(p.Ended)
	}
	return out
}

func ProcessInfoFrom(p *execenvv1.ProcessInfo) execenv.ProcessInfo {
	out := execenv.ProcessInfo{
		Tag:     p.GetTag(),
		PID:     int(p.GetPid()),
		PTY:     p.GetPty(),
		Running: p.GetRunning(),
		Exit:    ExitFrom(p.GetExit()),
		Output:  OffsetsFrom(p.GetOutput()),
		Input:   p.GetInput(),
	}
	if out.Output == nil {
		out.Output = map[execenv.Stream]uint64{}
	}
	if t := p.GetStarted(); t != nil {
		out.Started = t.AsTime()
	}
	if t := p.GetEnded(); t != nil {
		out.Ended = t.AsTime()
	}
	return out
}

var outputKinds = map[execenv.OutputKind]execenvv1.OutputKind{
	execenv.OutputData:      execenvv1.OutputKind_OUTPUT_KIND_DATA,
	execenv.OutputTruncated: execenvv1.OutputKind_OUTPUT_KIND_TRUNCATED,
	execenv.OutputExit:      execenvv1.OutputKind_OUTPUT_KIND_EXIT,
}

func OutputTo(o execenv.Output) *execenvv1.OutputEvent {
	out := &execenvv1.OutputEvent{Kind: outputKinds[o.Kind], Stream: string(o.Stream), Offset: o.Offset, To: o.To, Data: o.Data}
	if o.Kind == execenv.OutputExit {
		out.Exit = ExitTo(o.Exit)
	}
	return out
}

// OutputFrom converts an event; an unknown kind is false.
func OutputFrom(o *execenvv1.OutputEvent) (execenv.Output, bool) {
	for kind, wire := range outputKinds {
		if wire == o.GetKind() {
			return execenv.Output{Kind: kind, Stream: execenv.Stream(o.GetStream()), Offset: o.GetOffset(), To: o.GetTo(), Data: o.GetData(), Exit: ExitFrom(o.GetExit())}, true
		}
	}
	return execenv.Output{}, false
}
