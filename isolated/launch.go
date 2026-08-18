package isolated

import (
	"context"
	"sync"

	"github.com/sudosylabs/execenv"
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
type instance interface {
	Pause() error
	Resume() error
	Stop() error
}

type launcher interface {
	Start(ctx context.Context, req startRequest) (instance, error)
}

// stubInstance records lifecycle for tests. It does not start a process.
type stubInstance struct {
	mu      sync.Mutex
	paused  bool
	stopped bool
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
	return nil
}

type recordingLauncher struct {
	mu        sync.Mutex
	starts    int
	instances []*stubInstance
	last      startRequest
	fail      error
}

func (l *recordingLauncher) Start(_ context.Context, req startRequest) (instance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return nil, l.fail
	}
	l.starts++
	l.last = req
	inst := &stubInstance{}
	l.instances = append(l.instances, inst)
	return inst, nil
}
