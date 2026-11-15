package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
	"github.com/sudosylabs/execenv/internal/files"
	guestv1 "github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1"
	"github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1/guestv1connect"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/journal"
	"github.com/sudosylabs/execenv/internal/processes"
	workspace "github.com/sudosylabs/execenv/internal/watch"
	"github.com/sudosylabs/execenv/internal/wire"
	"github.com/sudosylabs/execenv/internal/workload"
)

// Config is one agent's occupancy of a projected tree.
type Config struct {
	// Home is the projected tree and the login shell's working directory.
	// The image root is not this directory and stays read-only.
	Home string
	// Scratch is the login shell's $HOME, outside the projected tree, so
	// tool caches and virtual environments are neither observed nor
	// harvested. Empty uses Home (local development only).
	Scratch string
	// DiskHome is Home reached through its disk's own mount, where the file
	// API works: a write staged on the disk is renamed into place, and a
	// rename never crosses mounts. A guest's Home is a bind mount of it.
	// Empty uses Home.
	DiskHome string
	// Content is where file bodies are kept: uploads waiting for the
	// projection and the bodies observations report. It must not be under
	// Home and should be on the same disk. Empty uses a temporary directory
	// (local development only).
	Content string
	// ContainWorkload requires a dedicated guest with a usable kernel write
	// barrier and launches every workload process unprivileged. Unsupported
	// configurations fail before serving or accepting a terminal. It is how
	// a real guest serves the journal.
	ContainWorkload bool
	// Barrier serves the journal without a contained workload. It must stop
	// every writer under Home while its function runs. Only tests whose
	// files are quiescent may supply one; a guest uses ContainWorkload.
	// Without either, the helper advertises no journal and every host
	// refuses it.
	Barrier WriteBarrier
	// Processes records the workload's processes without a contained
	// workload. Only tests supply one; Serve closes it when it returns. A
	// contained workload always reads the guest kernel's process events
	// and ignores this. A helper with neither advertises no process log,
	// and every host refuses it.
	Processes ProcessSource
}

// WriteBarrier stops every writer under Home while fn runs. An error from
// fn means the disk state is uncertain; the barrier stays closed after it.
type WriteBarrier = journal.Barrier

// Agent is the guest helper. It outlives any one host link: the workload,
// its processes, their recent output, the journal and the process log stay
// as they are while no host is connected, and the next link finds them.
type Agent struct {
	s       *server
	cancel  context.CancelFunc
	closers []func()
	serving sync.Mutex
	// mu guards stopLink, which ends the link being served.
	mu       sync.Mutex
	stopLink context.CancelFunc
}

// New prepares the helper's occupancy of cfg.Home. Unsupported
// configurations fail here, before any link is served.
func New(cfg Config) (*Agent, error) {
	if cfg.Home == "" {
		return nil, execenv.Error("agent", execenv.ErrInvalid)
	}
	a := &Agent{}
	ok := false
	defer func() {
		if !ok {
			a.Close()
		}
	}()
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return nil, execenv.Error("agent", err)
	}
	scratch := cfg.Scratch
	if scratch == "" {
		scratch = cfg.Home
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, execenv.Error("agent", err)
	}
	var group *workload.Group
	if cfg.ContainWorkload {
		var err error
		group, err = workload.Open()
		if err != nil {
			return nil, execenv.Error("workload", err)
		}
		a.closers = append(a.closers, func() { _ = group.Close() })
		if err := group.OwnHome(cfg.Home); err != nil {
			return nil, execenv.Error("workload", err)
		}
		if err := group.OwnHome(scratch); err != nil {
			return nil, execenv.Error("workload", err)
		}
	}
	s := &server{
		workload: group,
		home:     cfg.Home,
		scratch:  scratch,
		log:      processes.New(),
		procs:    make(map[string]*guestProc),
	}
	a.s = s
	contentDir := cfg.Content
	if contentDir == "" {
		dir, err := os.MkdirTemp("", "execenv-content-")
		if err != nil {
			return nil, execenv.Error("agent", err)
		}
		a.closers = append(a.closers, func() { _ = os.RemoveAll(dir) })
		contentDir = dir
	}
	store, err := content.Open(contentDir)
	if err != nil {
		return nil, execenv.Error("agent", err)
	}
	switch {
	case group != nil:
		s.journal = journal.New(cfg.Home, store, group, group.OwnHome)
	case cfg.Barrier != nil:
		s.journal = journal.New(cfg.Home, store, cfg.Barrier, nil)
	}
	// File API writes stage beside the content store, on the same disk.
	fileHome := cfg.Home
	if cfg.DiskHome != "" {
		fileHome = cfg.DiskHome
	}
	fs, err := files.New(fileHome, filepath.Join(contentDir, ".files"))
	if err != nil {
		return nil, execenv.Error("agent", err)
	}
	if group != nil {
		fs.Own = group.OwnHome
		// A real guest watches its workspace, so an idle one is not walked.
		if w, err := workspace.New(cfg.Home); err == nil && s.journal != nil {
			s.journal.Watch(w)
			a.closers = append(a.closers, func() { _ = w.Close() })
		}
	}
	s.files = fs
	if s.journal != nil {
		a.closers = append(a.closers, func() { s.journal.Close() })
	}
	// Processes stop before anything they write to closes.
	a.closers = append(a.closers, s.stopAll)
	// The process log must be recording before any workload process can
	// start. Without the kernel's process events the helper says so in
	// its handshake and the host refuses it.
	source := cfg.Processes
	if group != nil {
		source = nil
		if kernel, err := openKernelProcesses(group.Path(), cfg.Home, scratch, s.launchOf); err == nil {
			source = kernel
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	if source != nil {
		s.recordProcesses(ctx, source)
	}
	ok = true
	return a, nil
}

// Serve handles one host link until it ends or ctx does. Losing the link
// is a hangup: everything the helper holds stays for the next link. One
// link is served at a time, and a new one takes over from the old: a host
// that dials again has replaced the one before, which may have died
// without the link noticing.
func (a *Agent) Serve(ctx context.Context, conn net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	if a.stopLink != nil {
		a.stopLink()
	}
	a.stopLink = cancel
	a.mu.Unlock()
	a.serving.Lock()
	defer a.serving.Unlock()
	if ctx.Err() != nil {
		// A newer link took over while this one waited.
		return nil
	}
	a.s.serveLink(ctx, conn, a.s)
	return nil
}

// Close ends every process and the workload. The machine goes with it.
func (a *Agent) Close() {
	if a.cancel != nil {
		a.cancel()
	}
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

// Serve runs a helper for exactly one link: tests and one-shot guests.
func Serve(ctx context.Context, conn net.Conn, cfg Config) error {
	a, err := New(cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Serve(ctx, conn)
}

// serveLink serves handler on conn until either ends.
func (s *server) serveLink(ctx context.Context, conn net.Conn, handler guestv1connect.AgentServiceHandler) {
	link := watch(conn)
	mux := http.NewServeMux()
	mux.Handle(guestv1connect.NewAgentServiceHandler(handler))
	srv := linkServer(mux)
	served := make(chan struct{})
	go func() {
		_ = srv.Serve(&oneConn{conn: link})
		close(served)
	}()
	select {
	case <-ctx.Done():
	case <-link.done:
	}
	// The link closes first: the server's accept waits on it.
	_ = link.Close()
	_ = srv.Close()
	<-served
}

type server struct {
	// launches is each running process Start created, pid to launch, for
	// the process log.
	launches    sync.Map
	home        string
	scratch     string
	workload    *workload.Group
	workloadErr error
	journal     *journal.Journal
	files       *files.FS
	// processes is whether the process log source is recording; log keeps
	// its events until the host acknowledges them.
	processes atomic.Bool
	log       *processes.Log

	mu     sync.Mutex
	frozen bool
	procs  map[string]*guestProc
}

func (s *server) guard() error {
	if s.workload != nil {
		if err := s.workload.Check(); err != nil {
			return err
		}
	}
	if s.workloadErr != nil {
		return execenv.ErrUnavailable
	}
	if s.frozen {
		return execenv.ErrFrozen
	}
	return nil
}

func (s *server) Hello(context.Context, *connect.Request[guestv1.HelloRequest]) (*connect.Response[guestv1.HelloResponse], error) {
	return connect.NewResponse(&guestv1.HelloResponse{
		Protocol:  protocol,
		Journal:   s.journal != nil,
		Processes: s.processes.Load(),
	}), nil
}

func (s *server) Freeze(context.Context, *connect.Request[guestv1.FreezeRequest]) (*connect.Response[guestv1.FreezeResponse], error) {
	s.mu.Lock()
	s.frozen = true
	s.mu.Unlock()
	return connect.NewResponse(&guestv1.FreezeResponse{}), nil
}

func (s *server) Thaw(context.Context, *connect.Request[guestv1.ThawRequest]) (*connect.Response[guestv1.ThawResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workload != nil {
		if err := s.workload.Check(); err != nil {
			return nil, wire.Error(err)
		}
	}
	s.frozen = false
	return connect.NewResponse(&guestv1.ThawResponse{}), nil
}

// ObserveProcesses pages the process log the guest retains.
func (s *server) ObserveProcesses(_ context.Context, req *connect.Request[execenvv1.ObserveProcessesRequest]) (*connect.Response[execenvv1.ProcessPage], error) {
	page, err := s.log.Page(req.Msg.GetAfter(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ProcessPageTo(page)), nil
}

// AcknowledgeProcesses releases retained events through a sequence.
func (s *server) AcknowledgeProcesses(_ context.Context, req *connect.Request[execenvv1.AcknowledgeProcessesRequest]) (*connect.Response[execenvv1.AcknowledgeProcessesResponse], error) {
	if err := s.log.Acknowledge(req.Msg.GetThrough()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.AcknowledgeProcessesResponse{}), nil
}

func (s *server) journalGuard() error {
	if s.journal == nil {
		return execenv.ErrUnavailable
	}
	return s.guard()
}

func (s *server) UploadContent(_ context.Context, stream *connect.ClientStream[execenvv1.UploadContentRequest]) (*connect.Response[execenvv1.ContentReference], error) {
	if !stream.Receive() || stream.Msg().GetFence() == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	fence := wire.FenceFrom(stream.Msg().GetFence())
	if execenv.ValidateProjectionFence(fence) != nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	if s.journal == nil {
		return nil, wire.Error(execenv.ErrUnavailable)
	}
	// The body streams to the workspace disk without the server's lock;
	// only registering it takes the lock.
	body := &wire.ChunkReader{Next: func() ([]byte, bool, error) {
		if !stream.Receive() {
			return nil, false, stream.Err()
		}
		return stream.Msg().GetChunk(), true, nil
	}}
	blob, err := s.journal.Receive(body)
	if err != nil {
		return nil, wire.Error(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.journalGuard(); err != nil {
		return nil, wire.Error(err)
	}
	ref, err := s.journal.Upload(fence, blob)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ContentTo(&ref)), nil
}

func (s *server) InitializeProjection(ctx context.Context, req *connect.Request[execenvv1.InitializeProjectionRequest]) (*connect.Response[execenvv1.ProjectionReceipt], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.journalGuard(); err != nil {
		return nil, wire.Error(err)
	}
	if s.runningLocked() > 0 && !s.journal.Initialized() {
		return nil, wire.Error(execenv.ErrConflict)
	}
	receipt, err := s.journal.Initialize(ctx, wire.InitializationFrom(req.Msg))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ProjectionReceiptTo(receipt)), nil
}

func (s *server) ApplyProjection(ctx context.Context, req *connect.Request[execenvv1.ApplyProjectionRequest]) (*connect.Response[execenvv1.ProjectionReceipt], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.journalGuard(); err != nil {
		return nil, wire.Error(err)
	}
	receipt, err := s.journal.Apply(ctx, wire.ApplyFrom(req.Msg))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ProjectionReceiptTo(receipt)), nil
}

// bound checks the journal is served and binds the fence's epoch.
func (s *server) bound(fence *execenvv1.Fence) (execenv.ProjectionFence, error) {
	f := wire.FenceFrom(fence)
	if err := s.journalGuard(); err != nil {
		return f, err
	}
	if execenv.ValidateProjectionFence(f) != nil {
		return f, execenv.ErrInvalid
	}
	return f, s.journal.Bind(f)
}

func (s *server) Observe(ctx context.Context, req *connect.Request[execenvv1.ObserveRequest]) (*connect.Response[execenvv1.ObservationPage], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fence, err := s.bound(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	page, err := s.journal.Observe(ctx, fence, req.Msg.GetAfter(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ObservationPageTo(page)), nil
}

// ReadObservationContent streams the file an observation names, as it is
// now, ending with ErrStale when it is no longer the body observed. An
// unknown or retired observation is refused before anything is sent.
func (s *server) ReadObservationContent(_ context.Context, req *connect.Request[execenvv1.ReadObservationContentRequest], stream *connect.ServerStream[execenvv1.ContentChunk]) error {
	ref := wire.ContentFrom(req.Msg.GetContent())
	if ref == nil || execenv.ValidateContentReference(*ref) != nil {
		return wire.Error(execenv.ErrInvalid)
	}
	s.mu.Lock()
	fence, err := s.bound(req.Msg.GetFence())
	var body io.ReadCloser
	if err == nil {
		body, err = s.journal.Body(fence, *ref)
	}
	s.mu.Unlock()
	if err != nil {
		return wire.Error(err)
	}
	defer body.Close()
	buf := make([]byte, wire.ChunkSize)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if serr := stream.Send(&execenvv1.ContentChunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if errors.Is(err, execenv.ErrStale) {
			// The file changed after it was observed: a later observation
			// reports it.
			return wire.Error(err)
		}
		if err != nil {
			return wire.Error(execenv.ErrUnavailable)
		}
	}
}

func (s *server) ConfirmObservationProjection(_ context.Context, req *connect.Request[execenvv1.ConfirmObservationProjectionRequest]) (*connect.Response[execenvv1.ConfirmObservationProjectionResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.journalGuard(); err != nil {
		return nil, wire.Error(err)
	}
	binding := execenv.ObservationProjection{
		Fence:        wire.FenceFrom(req.Msg.GetFence()),
		HostSequence: req.Msg.GetHostSequence(),
		Mutation:     wire.MutationFrom(req.Msg.GetMutation()),
	}
	if err := s.journal.Confirm(binding); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.ConfirmObservationProjectionResponse{}), nil
}

func (s *server) AcknowledgeObservations(_ context.Context, req *connect.Request[execenvv1.AcknowledgeObservationsRequest]) (*connect.Response[execenvv1.AcknowledgeObservationsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fence, err := s.bound(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	if err := s.journal.Acknowledge(fence, req.Msg.GetThrough()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.AcknowledgeObservationsResponse{}), nil
}
