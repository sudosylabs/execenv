package isolated

import (
	"context"
	"net"
	"os"
	"sync"

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
	cancel    context.CancelFunc
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
	instances []*stubInstance
	last      startRequest
	fail      error
	// noJournal serves a helper that advertises no native journal.
	noJournal bool
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
	inst, err := startStubAgent(ctx, req, !l.noJournal)
	if err != nil {
		return nil, err
	}
	l.starts++
	l.last = req
	l.instances = append(l.instances, inst)
	return inst, nil
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

func startStubAgent(_ context.Context, req startRequest, journal bool) (*stubInstance, error) {
	if err := os.MkdirAll(req.TreeDir, 0o700); err != nil {
		return nil, err
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
	cfg := agent.Config{Home: req.TreeDir, Scratch: scratch}
	if journal {
		cfg.Barrier = quiescentBarrier{}
	}
	go func() {
		_ = agent.ListenAndServe(ctx, ln, cfg)
	}()
	return &stubInstance{sock: sock, scratch: scratch, ln: ln, cancel: cancel}, nil
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
