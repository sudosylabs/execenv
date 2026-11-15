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

	"github.com/sudosylabs/execenv"
	guestv1 "github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1"
	"github.com/sudosylabs/execenv/internal/wire"
)

// The guest runs the processes the host starts. It holds no history: the
// host does. Output waits in the kernel's pipe or terminal buffer, and in a
// short queue here, until the host reads it, so a process outruns no
// reader; it is held back instead.

// outputDrain is how long a process's output may still arrive after it
// exits, from children that kept its terminal or pipes open.
const outputDrain = 200 * time.Millisecond

type chunk struct {
	stream execenv.Stream
	data   []byte
	exit   *execenv.ExitStatus
}

type guestProc struct {
	spec  execenv.ProcessSpec
	cmd   *exec.Cmd
	pty   *os.File
	stdin *os.File
	// reads are the host ends of the outputs; they close after the drain.
	reads  []*os.File
	out    chan chunk
	exited chan struct{}

	// outMu admits one Output stream at a time; inMu one Feed.
	outMu sync.Mutex
	inMu  sync.Mutex
}

func (p *guestProc) done() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
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
	running := 0
	for _, p := range s.procs {
		if !p.done() {
			running++
		}
	}
	if running >= execenv.MaxProcesses {
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
	return connect.NewResponse(&guestv1.SpawnResponse{Pid: int32(p.cmd.Process.Pid)}), nil
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
	if s.workload != nil {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmd = exec.Command(self, append([]string{"workload-exec"}, argv...)...)
		if err := s.workload.Configure(cmd); err != nil {
			return nil, err
		}
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
	p := &guestProc{spec: spec, cmd: cmd, out: make(chan chunk, 16), exited: make(chan struct{})}
	if spec.PTY != nil {
		master, err := startPTY(cmd, *spec.PTY)
		if err != nil {
			return nil, err
		}
		p.pty = master
		p.reads = []*os.File{master}
		go p.pump(execenv.StreamPTY, master)
	} else if err := p.startPiped(); err != nil {
		return nil, err
	}
	go p.wait()
	return p, nil
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
	for _, stream := range []execenv.Stream{execenv.StreamStdout, execenv.StreamStderr} {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(err)
		}
		p.reads = append(p.reads, r)
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
	for _, f := range closeAfter {
		_ = f.Close()
	}
	go p.pump(execenv.StreamStdout, p.reads[0])
	go p.pump(execenv.StreamStderr, p.reads[1])
	return nil
}

func (p *guestProc) pump(stream execenv.Stream, r io.Reader) {
	buf := make([]byte, wire.ChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			select {
			case p.out <- chunk{stream: stream, data: append([]byte(nil), buf[:n]...)}:
			case <-p.exited:
				// After the exit, output waits only for the drain.
				select {
				case p.out <- chunk{stream: stream, data: append([]byte(nil), buf[:n]...)}:
				case <-time.After(outputDrain):
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// wait reports the exit once the process has ended and its output has
// drained. Children left holding its outputs are cut off after the drain.
func (p *guestProc) wait() {
	_ = p.cmd.Wait()
	status := exitStatus(p.cmd)
	close(p.exited)
	time.Sleep(outputDrain)
	for _, f := range p.reads {
		_ = f.Close()
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	p.out <- chunk{exit: &status}
	close(p.out)
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

// Output streams the process's output, then its exit. A stream that ends
// early loses at most the chunk in flight; the host opens another.
func (s *server) Output(ctx context.Context, req *connect.Request[guestv1.OutputRequest], stream *connect.ServerStream[guestv1.OutputResponse]) error {
	p, err := s.proc(req.Msg.GetTag())
	if err != nil {
		return wire.Error(err)
	}
	if !p.outMu.TryLock() {
		return wire.Error(execenv.ErrBusy)
	}
	defer p.outMu.Unlock()
	if err := stream.Send(&guestv1.OutputResponse{Event: &guestv1.OutputResponse_Opened{Opened: &guestv1.Empty{}}}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c, ok := <-p.out:
			if !ok {
				return nil
			}
			var msg *guestv1.OutputResponse
			if c.exit != nil {
				msg = &guestv1.OutputResponse{Event: &guestv1.OutputResponse_Exit{Exit: wire.ExitTo(*c.exit)}}
			} else {
				msg = &guestv1.OutputResponse{Event: &guestv1.OutputResponse_Data{Data: &guestv1.Chunk{Stream: string(c.stream), Data: c.data}}}
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

func (p *guestProc) input() io.Writer {
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
	w := p.input()
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
		if _, err := w.Write(stream.Msg().GetData()); err != nil {
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
	return connect.NewResponse(&guestv1.Empty{}), nil
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

// runningLocked reports whether any process is still running.
func (s *server) runningLocked() bool {
	for _, p := range s.procs {
		if !p.done() {
			return true
		}
	}
	return false
}

// stopAll kills every process. A contained workload is killed whole, so
// nothing a process left behind survives the link.
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
