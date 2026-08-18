package isolated

import (
	"context"
	"net"
	"os"
	"sync"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
)

// workspaceName is the grant-relative directory that holds the projected
// tree. The isolated environment treats this path as home and cwd; the
// root filesystem of the image stays read-only.
const workspaceName = "workspace"

type startRequest struct {
	ID      execenv.ID
	Kernel  string
	Rootfs  string
	TreeDir string
	Network execenv.Network
	Memory  int64
	CPU     int
}

// instance is one running isolated environment. Pause must not destroy it.
// Connect is the guest link the host dials after Start; the public Host
// type does not name the transport.
type instance interface {
	Pause() error
	Resume() error
	Stop() error
	Connect(ctx context.Context) (net.Conn, error)
}

type launcher interface {
	Start(ctx context.Context, req startRequest) (instance, error)
}

// stubInstance records lifecycle and serves a local agent on a Unix
// socket so unit tests never need a microVM.
type stubInstance struct {
	mu      sync.Mutex
	paused  bool
	stopped bool
	sock    string
	ln      net.Listener
	cancel  context.CancelFunc
}

func (s *stubInstance) Pause() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
	return nil
}

func (s *stubInstance) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = false
	return nil
}

func (s *stubInstance) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
}

func (l *recordingLauncher) Start(ctx context.Context, req startRequest) (instance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return nil, l.fail
	}
	inst, err := startStubAgent(ctx, req)
	if err != nil {
		return nil, err
	}
	l.starts++
	l.last = req
	l.instances = append(l.instances, inst)
	return inst, nil
}

func startStubAgent(_ context.Context, req startRequest) (*stubInstance, error) {
	if err := os.MkdirAll(req.TreeDir, 0o700); err != nil {
		return nil, err
	}
	// Temp-dir unix sockets stay short enough for the platform path limit
	// and stay unique across parallel tests.
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
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = agent.ListenAndServe(ctx, ln, agent.Config{Home: req.TreeDir})
	}()
	return &stubInstance{sock: sock, ln: ln, cancel: cancel}, nil
}
