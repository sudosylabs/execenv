package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sudosylabs/execenv"
	guestv1 "github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/ring"
	"github.com/sudosylabs/execenv/internal/screen"
	"github.com/sudosylabs/execenv/internal/wire"
)

// The guest runs the processes the host starts and keeps the newest output
// of each stream by absolute offset. A process may run ahead of what the
// host has read by at most guestHistory per stream; beyond that it is held
// back, so nothing the host has not received is ever dropped. A host that
// restarts lists the processes and reads on from where the guest stands.

const (
	// guestHistory is the output each stream keeps. It covers everything
	// the guest link can hold in flight, so a broken link loses nothing.
	guestHistory = 2 << 20
	// outputDrain is how long a process's output may still arrive after it
	// exits, from children that kept its terminal or pipes open.
	outputDrain = 200 * time.Millisecond
)

type guestProc struct {
	// untag forgets its pid's tag once the process is gone.
	untag func()
	// screen is a terminal process's headless terminal, fed every byte it
	// writes, in order, by its pump.
	screen  *screen.Terminal
	spec    execenv.ProcessSpec
	cmd     *exec.Cmd
	started time.Time
	pty     *os.File
	stdin   *os.File
	// reads are the guest ends of the outputs; they close after the drain.
	reads []*os.File

	mu   sync.Mutex
	cond *sync.Cond
	// rings hold each stream's newest output; sent is how far the current
	// reader has been sent. A stream's pump waits while it is guestHistory
	// ahead of sent.
	rings   map[execenv.Stream]*ring.Ring
	sent    map[execenv.Stream]uint64
	pumping int
	closing bool
	exited  bool
	ended   time.Time
	exit    execenv.ExitStatus
	input   uint64

	// outMu admits one Output stream at a time; inMu one Feed.
	outMu sync.Mutex
	inMu  sync.Mutex
}

func (p *guestProc) done() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

func (s *server) Spawn(_ context.Context, req *connect.Request[guestv1.SpawnRequest]) (*connect.Response[guestv1.SpawnResponse], error) {
	spec, ok := wire.ProcessSpecFrom(req.Msg.GetSpec())
	if !ok || execenv.ValidateProcessSpec(spec) != nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(); err != nil {
		return nil, wire.Error(err)
	}
	if old, ok := s.procs[spec.Tag]; ok && !old.done() {
		return nil, wire.Error(execenv.ErrConflict)
	}
	if s.runningLocked() >= execenv.MaxProcesses {
		return nil, wire.Error(execenv.ErrBusy)
	}
	p, err := s.spawn(spec)
	if err != nil {
		if errors.Is(err, execenv.ErrInvalid) || errors.Is(err, execenv.ErrNotFound) {
			return nil, wire.Error(err)
		}
		return nil, wire.Error(execenv.ErrUnavailable)
	}
	s.procs[spec.Tag] = p
	return connect.NewResponse(&guestv1.SpawnResponse{Pid: int32(p.cmd.Process.Pid), Started: timestamppb.New(p.started)}), nil
}

// spawn starts spec in the workload: unprivileged and in its cgroup when
// the workload is contained, with the workspace as its directory and the
// scratch as $HOME.
func (s *server) spawn(spec execenv.ProcessSpec) (*guestProc, error) {
	argv := spec.Argv
	if len(argv) == 0 {
		argv = []string{"/bin/sh", "-l"}
	}
	var cmd *exec.Cmd
	// release lets a workload process run its command once the helper has
	// recorded it and its tag.
	var release, held *os.File
	defer func() {
		if held != nil {
			_ = held.Close()
		}
		if release != nil {
			_ = release.Close()
		}
	}()
	if s.workload != nil {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmd = exec.Command(self, append([]string{"workload-exec"}, argv...)...)
		if err := s.workload.Configure(cmd); err != nil {
			return nil, err
		}
		held, release, err = os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.ExtraFiles = []*os.File{held}
	} else {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			return nil, execenv.ErrNotFound
		}
		cmd = exec.Command(path, argv[1:]...)
	}
	dir := s.home
	if spec.Dir != "" {
		dir = filepath.Join(s.home, filepath.FromSlash(spec.Dir))
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return nil, execenv.ErrNotFound
		}
	}
	cmd.Dir = dir
	cmd.Env = environment(s.scratch, dir, spec)
	p := &guestProc{spec: spec, cmd: cmd, rings: map[execenv.Stream]*ring.Ring{}, sent: map[execenv.Stream]uint64{}}
	p.cond = sync.NewCond(&p.mu)
	if spec.PTY != nil {
		master, err := startPTY(cmd, *spec.PTY)
		if err != nil {
			return nil, err
		}
		p.pty = master
		p.reads = []*os.File{master}
		p.started = time.Now()
		p.screen = screen.New(*spec.PTY)
		p.pumpAll(map[execenv.Stream]*os.File{execenv.StreamPTY: master})
	} else if err := p.startPiped(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	l := &launch{tag: spec.Tag, argv: argv, dir: cmd.Dir}
	s.launches.Store(pid, l)
	p.untag = func() { s.launches.CompareAndDelete(pid, l) }
	if release != nil {
		_ = held.Close()
		held = nil
		_, _ = release.Write([]byte{1})
	}
	go p.wait()
	return p, nil
}

// launchOf is the process Start created with pid.
func (s *server) launchOf(pid int) (launch, bool) {
	v, ok := s.launches.Load(pid)
	if !ok {
		return launch{}, false
	}
	return *v.(*launch), true
}

// environment is the workload's base environment with the spec's added.
func environment(scratch, dir string, spec execenv.ProcessSpec) []string {
	base := map[string]string{
		"HOME": scratch,
		"PWD":  dir,
		"PATH": os.Getenv("PATH"),
	}
	if spec.PTY != nil {
		base["TERM"] = "xterm-256color"
	}
	for k, v := range spec.Env {
		base[k] = v
	}
	keys := make([]string, 0, len(base))
	for k := range base {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+base[k])
	}
	return env
}

func (p *guestProc) startPiped() error {
	var closeAfter []*os.File
	fail := func(err error) error {
		for _, f := range append(closeAfter, p.reads...) {
			_ = f.Close()
		}
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		return err
	}
	if p.spec.Stdin {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		p.cmd.Stdin, p.stdin = r, w
		closeAfter = append(closeAfter, r)
	}
	outputs := map[execenv.Stream]*os.File{}
	for _, stream := range []execenv.Stream{execenv.StreamStdout, execenv.StreamStderr} {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		p.reads = append(p.reads, r)
		outputs[stream] = r
		closeAfter = append(closeAfter, w)
		if stream == execenv.StreamStdout {
			p.cmd.Stdout = w
		} else {
			p.cmd.Stderr = w
		}
	}
	// Its own session, so a signal reaches the whole job.
	newSession(p.cmd)
	if err := p.cmd.Start(); err != nil {
		return fail(err)
	}
	p.started = time.Now()
	for _, f := range closeAfter {
		_ = f.Close()
	}
	p.pumpAll(outputs)
	return nil
}

func (p *guestProc) pumpAll(outputs map[execenv.Stream]*os.File) {
	p.mu.Lock()
	for name := range outputs {
		p.rings[name] = ring.New(guestHistory, 0)
		p.pumping++
	}
	p.mu.Unlock()
	for name, f := range outputs {
		go p.pump(name, f)
	}
}

// pump moves a stream's output into its ring, holding the process back
// while the ring is a full history ahead of the host.
func (p *guestProc) pump(stream execenv.Stream, r io.Reader) {
	buf := make([]byte, wire.ChunkSize)
	defer func() {
		if stream == execenv.StreamPTY && p.screen != nil {
			// Its output is over; the screen stays readable.
			p.screen.Close()
		}
		p.mu.Lock()
		p.pumping--
		p.cond.Broadcast()
		p.mu.Unlock()
	}()
	for {
		n, err := r.Read(buf)
		if n > 0 {
			p.mu.Lock()
			rg := p.rings[stream]
			for !p.closing && rg.Total()+uint64(n)-p.sent[stream] > guestHistory {
				p.cond.Wait()
			}
			if p.closing && rg.Total()+uint64(n)-p.sent[stream] > guestHistory {
				// Past the drain, output nobody reads is not kept.
				p.mu.Unlock()
				return
			}
			rg.Write(buf[:n])
			p.cond.Broadcast()
			p.mu.Unlock()
			if stream == execenv.StreamPTY && p.screen != nil {
				// The only writer, so the screen takes output in the
				// ring's order and its offset matches the ring's.
				_, _ = p.screen.Write(buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

// wait records the exit, then cuts off children left holding the outputs
// once the drain has passed.
func (p *guestProc) wait() {
	_ = p.cmd.Wait()
	ended := time.Now()
	status := exitStatus(p.cmd)
	p.mu.Lock()
	p.exited, p.exit, p.ended = true, status, ended
	p.cond.Broadcast()
	p.mu.Unlock()
	time.Sleep(outputDrain)
	if p.untag != nil {
		// Long after the process log read its start.
		p.untag()
	}
	p.mu.Lock()
	p.closing = true
	p.cond.Broadcast()
	p.mu.Unlock()
	for _, f := range p.reads {
		_ = f.Close()
	}
	if p.stdin != nil {
		p.inMu.Lock()
		_ = p.stdin.Close()
		p.inMu.Unlock()
	}
}

func exitStatus(cmd *exec.Cmd) execenv.ExitStatus {
	state := cmd.ProcessState
	if state == nil {
		return execenv.ExitStatus{Code: -1}
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return execenv.ExitStatus{Signal: int(ws.Signal())}
	}
	return execenv.ExitStatus{Code: state.ExitCode()}
}

func (s *server) proc(tag string) (*guestProc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[tag]
	if !ok {
		return nil, execenv.ErrNotFound
	}
	return p, nil
}

// ListProcesses describes every process held, running or exited.
func (s *server) ListProcesses(context.Context, *connect.Request[guestv1.ListProcessesRequest]) (*connect.Response[guestv1.ListProcessesResponse], error) {
	s.mu.Lock()
	procs := make([]*guestProc, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.Unlock()
	out := &guestv1.ListProcessesResponse{}
	for _, p := range procs {
		out.Processes = append(out.Processes, p.describe())
	}
	return connect.NewResponse(out), nil
}

func (p *guestProc) describe() *guestv1.GuestProcess {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := execenv.ProcessInfo{Tag: p.spec.Tag, PID: p.cmd.Process.Pid, PTY: p.pty != nil, Started: p.started, Ended: p.ended, Running: !p.exited, Exit: p.exit, Input: p.input, Output: map[execenv.Stream]uint64{}}
	first := map[execenv.Stream]uint64{}
	for name, r := range p.rings {
		info.Output[name] = r.Total()
		first[name] = r.First()
	}
	return &guestv1.GuestProcess{Spec: wire.ProcessSpecTo(p.spec), Info: wire.ProcessInfoTo(info), First: wire.OffsetsTo(first)}
}

// Output streams the process's output from the offsets asked for, then
// its exit once every stream has drained. The reader has everything
// before its offsets, so the guest may drop it.
func (s *server) Output(ctx context.Context, req *connect.Request[guestv1.OutputRequest], stream *connect.ServerStream[guestv1.OutputResponse]) error {
	p, err := s.proc(req.Msg.GetTag())
	if err != nil {
		return wire.Error(err)
	}
	if !p.outMu.TryLock() {
		return wire.Error(execenv.ErrBusy)
	}
	defer p.outMu.Unlock()
	from := wire.OffsetsFrom(req.Msg.GetFrom())
	at := map[execenv.Stream]uint64{}
	p.mu.Lock()
	for name, r := range p.rings {
		pos, ok := from[name]
		if !ok || pos < r.First() {
			pos = r.First()
		}
		if pos > r.Total() {
			p.mu.Unlock()
			return wire.Error(execenv.ErrInvalid)
		}
		at[name] = pos
		p.sent[name] = pos
	}
	p.cond.Broadcast()
	names := make([]execenv.Stream, 0, len(at))
	for name := range at {
		names = append(names, name)
	}
	p.mu.Unlock()
	if err := stream.Send(&guestv1.OutputResponse{Event: &guestv1.OutputResponse_Opened{Opened: &guestv1.Empty{}}}); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer stop()
	next := 0
	for {
		p.mu.Lock()
		var msg *guestv1.OutputResponse
		for msg == nil {
			if err := ctx.Err(); err != nil {
				p.mu.Unlock()
				return err
			}
			for range names {
				name := names[next%len(names)]
				next++
				if data := p.rings[name].Read(at[name], wire.ChunkSize); len(data) > 0 {
					msg = &guestv1.OutputResponse{Event: &guestv1.OutputResponse_Data{Data: &guestv1.Chunk{Stream: string(name), Data: data, Offset: at[name]}}}
					at[name] += uint64(len(data))
					break
				}
			}
			if msg == nil && p.exited && p.pumping == 0 {
				msg = &guestv1.OutputResponse{Event: &guestv1.OutputResponse_Exit{Exit: wire.ExitTo(p.exit)}, Ended: timestamppb.New(p.ended)}
			}
			if msg == nil {
				p.cond.Wait()
			}
		}
		p.mu.Unlock()
		if err := stream.Send(msg); err != nil {
			return err
		}
		if msg.GetExit() != nil {
			return nil
		}
		// What was sent is the reader's: the pump may run that far ahead.
		if data := msg.GetData(); data != nil {
			p.mu.Lock()
			name := execenv.Stream(data.GetStream())
			p.sent[name] = data.GetOffset() + uint64(len(data.GetData()))
			p.cond.Broadcast()
			p.mu.Unlock()
		}
	}
}

func (p *guestProc) inputWriter() io.Writer {
	if p.pty != nil {
		return p.pty
	}
	if p.stdin != nil {
		return p.stdin
	}
	return nil
}

// Feed writes input to the process the first message names, in order.
func (s *server) Feed(_ context.Context, stream *connect.ClientStream[guestv1.FeedRequest]) (*connect.Response[guestv1.FeedResponse], error) {
	if !stream.Receive() || stream.Msg().GetTag() == "" {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	p, err := s.proc(stream.Msg().GetTag())
	if err != nil {
		return nil, wire.Error(err)
	}
	w := p.inputWriter()
	if w == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	p.inMu.Lock()
	defer p.inMu.Unlock()
	for stream.Receive() {
		s.mu.Lock()
		err := s.guard()
		s.mu.Unlock()
		if err != nil {
			return nil, wire.Error(err)
		}
		n, err := w.Write(stream.Msg().GetData())
		p.mu.Lock()
		p.input += uint64(n)
		p.mu.Unlock()
		if err != nil {
			return nil, wire.Error(execenv.ErrClosed)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&guestv1.FeedResponse{}), nil
}

func (s *server) live(tag string) (*guestProc, error) {
	s.mu.Lock()
	err := s.guard()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	p, err := s.proc(tag)
	if err != nil {
		return nil, err
	}
	if p.done() {
		return nil, execenv.ErrClosed
	}
	return p, nil
}

func (s *server) CloseFeed(_ context.Context, req *connect.Request[guestv1.ProcessRef]) (*connect.Response[guestv1.Empty], error) {
	p, err := s.live(req.Msg.GetTag())
	if err != nil {
		return nil, wire.Error(err)
	}
	if p.stdin != nil {
		p.inMu.Lock()
		_ = p.stdin.Close()
		p.inMu.Unlock()
	}
	return connect.NewResponse(&guestv1.Empty{}), nil
}

func (s *server) ResizeProcess(_ context.Context, req *connect.Request[guestv1.ResizeRequest]) (*connect.Response[guestv1.Empty], error) {
	window, ok := wire.WindowFrom(req.Msg.GetWindow())
	if !ok {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	p, err := s.live(req.Msg.GetTag())
	if err != nil {
		return nil, wire.Error(err)
	}
	if p.pty == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	if err := setWindow(p.pty, window); err != nil {
		return nil, wire.Error(execenv.ErrUnavailable)
	}
	p.screen.Resize(window)
	return connect.NewResponse(&guestv1.Empty{}), nil
}

// ProcessScreen reads a terminal process's screen, running or exited.
func (s *server) ProcessScreen(_ context.Context, req *connect.Request[guestv1.ProcessScreenRequest]) (*connect.Response[execenvv1.Screen], error) {
	p, err := s.proc(req.Msg.GetTag())
	if err != nil {
		return nil, wire.Error(err)
	}
	if p.screen == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	return connect.NewResponse(wire.ScreenTo(p.screen.Snapshot())), nil
}

// SignalProcess signals the process's whole session.
func (s *server) SignalProcess(_ context.Context, req *connect.Request[guestv1.SignalRequest]) (*connect.Response[guestv1.Empty], error) {
	sig, ok := signals[execenv.Signal(req.Msg.GetSignal())]
	if !ok {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	p, err := s.live(req.Msg.GetTag())
	if err != nil {
		return nil, wire.Error(err)
	}
	signalSession(p.cmd, sig)
	return connect.NewResponse(&guestv1.Empty{}), nil
}

// runningLocked counts the processes still running.
func (s *server) runningLocked() int {
	n := 0
	for _, p := range s.procs {
		if !p.done() {
			n++
		}
	}
	return n
}

// stopAll kills every process. A contained workload is killed whole, so
// nothing a process left behind survives the helper.
func (s *server) stopAll() {
	s.mu.Lock()
	procs := make([]*guestProc, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.Unlock()
	for _, p := range procs {
		if !p.done() {
			signalSession(p.cmd, syscall.SIGKILL)
		}
	}
	if s.workload != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.workload.Kill(ctx)
		cancel()
	}
}
