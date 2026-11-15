package isolated

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
)

// stubInstance records lifecycle and serves a local agent on a Unix
// socket so unit tests never need a microVM.
type stubInstance struct {
	mu        sync.Mutex
	paused    bool
	pauseErr  error
	resumeErr error
	stopErr   error
	stopped   bool
	sock      string
	scratch   string
	ln        net.Listener
	conns     *trackedConns
	cancel    context.CancelFunc
	// processes simulates the guest kernel's view of the workload.
	processes *stubProcesses
	// exited closes when the simulated machine ends: Stop, or die.
	exited   chan struct{}
	exitOnce sync.Once
	// limits is what UpdateLimits last applied.
	limits execenv.Limits
	// saved is where Save put the machine; its helper keeps running, as the
	// saved memory would hold it, until a Start restores it.
	saved string
}

// Save stands for saving the machine: its link breaks, as a real save
// resets the guest link, and its helper waits for a restore.
func (s *stubInstance) Save(_ context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{jailState, jailMemory} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.saved = dir
	s.mu.Unlock()
	s.dropLink()
	return nil
}

func (s *stubInstance) Done() <-chan struct{} { return s.exited }

// Machine names the stub by its socket, which a restarted host finds again.
func (s *stubInstance) Machine() machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return machine{Sock: s.sock}
}

func (s *stubInstance) UpdateLimits(_ context.Context, limits execenv.Limits, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return execenv.ErrUnavailable
	}
	s.limits = limits
	return nil
}

// dropLink breaks the guest link while the simulated machine keeps
// running, as a guest helper that died would.
func (s *stubInstance) dropLink() { s.conns.closeAll() }

// trackedConns lets a test sever the links the stub helper accepted.
type trackedConns struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackedConns) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, conn)
		l.mu.Unlock()
	}
	return conn, err
}

func (l *trackedConns) closeAll() {
	l.mu.Lock()
	conns := l.conns
	l.conns = nil
	l.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// die ends the simulated machine on its own, as a killed monitor or a
// crashed guest would: the guest helper and its link go away, then the
// machine is reaped.
func (s *stubInstance) die() {
	s.dropLink()
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.ln != nil {
		_ = s.ln.Close()
		s.ln = nil
	}
	s.mu.Unlock()
	s.exitOnce.Do(func() { close(s.exited) })
}

func (s *stubInstance) Pause(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pauseErr != nil {
		return s.pauseErr
	}
	s.paused = true
	return nil
}

func (s *stubInstance) Resume(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resumeErr != nil {
		return s.resumeErr
	}
	s.paused = false
	return nil
}

func (s *stubInstance) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopErr != nil {
		return s.stopErr
	}
	s.stopped = true
	if s.saved != "" {
		// The saved machine is gone from memory but not lost.
		s.exitOnce.Do(func() { close(s.exited) })
		return nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.ln != nil {
		_ = s.ln.Close()
		s.ln = nil
	}
	if s.sock != "" {
		_ = os.Remove(s.sock)
	}
	if s.scratch != "" {
		_ = os.RemoveAll(s.scratch)
	}
	s.exitOnce.Do(func() { close(s.exited) })
	return nil
}

func (s *stubInstance) Connect(ctx context.Context) (net.Conn, error) {
	s.mu.Lock()
	sock := s.sock
	s.mu.Unlock()
	if sock == "" {
		return nil, execenv.ErrUnavailable
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", sock)
}

type recordingLauncher struct {
	mu        sync.Mutex
	starts    int
	restores  int
	instances []*stubInstance
	last      startRequest
	fail      error
	// noJournal serves a helper that advertises no native journal.
	noJournal bool
	// noProcesses serves a helper whose kernel has no process events.
	noProcesses bool
	byID        map[execenv.ID]*stubInstance
}

type blockingLauncher struct {
	inner   recordingLauncher
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *blockingLauncher) Start(ctx context.Context, req startRequest) (instance, error) {
	l.once.Do(func() { close(l.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.release:
		return l.inner.Start(ctx, req)
	}
}

func (l *recordingLauncher) Start(ctx context.Context, req startRequest) (instance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return nil, l.fail
	}
	if req.Restore != nil && !req.Restore.Start {
		for _, inst := range l.instances {
			inst.mu.Lock()
			if inst.saved == req.Restore.Dir {
				inst.saved, inst.stopped = "", false
				inst.exited, inst.exitOnce = make(chan struct{}), sync.Once{}
				inst.mu.Unlock()
				l.restores++
				l.last = req
				return inst, nil
			}
			inst.mu.Unlock()
		}
		return nil, execenv.ErrUnavailable
	}
	inst, err := startStubAgent(ctx, req, !l.noJournal, !l.noProcesses)
	if err != nil {
		return nil, err
	}
	l.starts++
	l.last = req
	l.instances = append(l.instances, inst)
	if l.byID == nil {
		l.byID = make(map[execenv.ID]*stubInstance)
	}
	l.byID[req.ID] = inst
	return inst, nil
}

// Adopt finds a stub machine a previous host started, if it still runs.
func (l *recordingLauncher) Adopt(_ context.Context, m machine) (instance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, inst := range l.instances {
		select {
		case <-inst.exited:
			continue
		default:
		}
		if inst.Machine().Sock == m.Sock {
			return inst, nil
		}
	}
	return nil, execenv.ErrLost
}

func (l *blockingLauncher) Adopt(ctx context.Context, m machine) (instance, error) {
	return l.inner.Adopt(ctx, m)
}

// instance returns the machine last started for id.
func (l *recordingLauncher) instance(id execenv.ID) *stubInstance {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byID[id]
}

// quiescentBarrier serves the stub helper's journal. Unit tests write the
// workspace only between calls, so nothing races a capture.
type quiescentBarrier struct{}

func (quiescentBarrier) Stopped(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func startStubAgent(_ context.Context, req startRequest, journal, processLog bool) (*stubInstance, error) {
	if err := os.MkdirAll(req.TreeDir, 0o700); err != nil {
		return nil, err
	}
	// The stub has no disk image: it plants the seed's tree directly.
	if req.Seed != "" {
		if err := os.CopyFS(req.TreeDir, os.DirFS(filepath.Join(req.Seed, "tree"))); err != nil {
			return nil, err
		}
	}
	f, err := os.CreateTemp("", "e*.sock")
	if err != nil {
		return nil, err
	}
	sock := f.Name()
	_ = f.Close()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	// The shell's HOME stays outside the grant directory, as on a real guest,
	// so a hung-up shell writing its history cannot race grant removal.
	scratch, err := os.MkdirTemp("", "execenv-scratch-")
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	processes := newStubProcesses()
	cfg := agent.Config{Home: req.TreeDir, Scratch: scratch}
	if journal {
		cfg.Barrier = quiescentBarrier{}
	}
	if processLog {
		cfg.Processes = processes
	}
	conns := &trackedConns{Listener: ln}
	go func() {
		_ = agent.ListenAndServe(ctx, conns, cfg)
	}()
	return &stubInstance{sock: sock, scratch: scratch, ln: ln, conns: conns, cancel: cancel, processes: processes, exited: make(chan struct{})}, nil
}

// stubProcesses is the stub helper's process source. Run reports one
// command as the guest kernel would see it start and exit, with a fresh PID.
type stubProcesses struct {
	events chan execenv.ProcessEvent
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	pid    int
}

func newStubProcesses() *stubProcesses {
	return &stubProcesses{events: make(chan execenv.ProcessEvent), done: make(chan struct{}), pid: 100}
}

func (p *stubProcesses) Next() (execenv.ProcessEvent, error) {
	select {
	case event := <-p.events:
		return event, nil
	case <-p.done:
		return execenv.ProcessEvent{}, execenv.ErrClosed
	}
}

func (p *stubProcesses) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *stubProcesses) Run(ctx context.Context, argv []string) error {
	return p.report(ctx, "", argv)
}

// Start reports a process the helper started under tag, as its kernel
// process tracker would.
func (p *stubProcesses) Start(ctx context.Context, tag string, argv []string) error {
	return p.report(ctx, tag, argv)
}

func (p *stubProcesses) report(ctx context.Context, tag string, argv []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pid++
	code := 0
	for _, event := range []execenv.ProcessEvent{
		{Kind: execenv.ProcessStarted, PID: p.pid, ParentPID: 100, Argv: argv, Tag: tag, At: time.Now()},
		{Kind: execenv.ProcessExited, PID: p.pid, ExitCode: &code, At: time.Now()},
	} {
		select {
		case p.events <- event:
		case <-p.done:
			return execenv.ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// recordingAttacher is the test adapter for attach: it records dests and
// returns a planned NIC without creating a tap.
type recordingAttacher struct {
	mu     sync.Mutex
	setups int
	dests  []string
	last   *netAttach
}

func (a *recordingAttacher) Setup(id execenv.ID, dests []string) (*netAttach, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setups++
	a.dests = append([]string(nil), dests...)
	allow, err := parseAllow(dests)
	if err != nil {
		return nil, err
	}
	att := planAttach(id, allow)
	a.last = &att
	return &att, nil
}
