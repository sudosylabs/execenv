package agent

import (
	"context"
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

// Serve handles one host connection until it ends. Losing the connection
// is hangup: Home stays. The caller tears the machine down separately.
func Serve(ctx context.Context, conn net.Conn, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.Home == "" {
		return execenv.Error("agent", execenv.ErrInvalid)
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return execenv.Error("agent", err)
	}
	scratch := cfg.Scratch
	if scratch == "" {
		scratch = cfg.Home
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return execenv.Error("agent", err)
	}
	var group *workload.Group
	if cfg.ContainWorkload {
		var err error
		group, err = workload.Open()
		if err != nil {
			return execenv.Error("workload", err)
		}
		defer group.Close()
		if err := group.OwnHome(cfg.Home); err != nil {
			return execenv.Error("workload", err)
		}
		if err := group.OwnHome(scratch); err != nil {
			return execenv.Error("workload", err)
		}
	}
	s := &server{
		workload: group,
		home:     cfg.Home,
		scratch:  scratch,
		events:   make(chan []execenv.ProcessEvent),
		procs:    make(map[string]*guestProc),
	}
	contentDir := cfg.Content
	if contentDir == "" {
		dir, err := os.MkdirTemp("", "execenv-content-")
		if err != nil {
			return execenv.Error("agent", err)
		}
		defer os.RemoveAll(dir)
		contentDir = dir
	}
	store, err := content.Open(contentDir)
	if err != nil {
		return execenv.Error("agent", err)
	}
	switch {
	case group != nil:
		s.journal = journal.New(cfg.Home, store, group, group.OwnHome)
	case cfg.Barrier != nil:
		s.journal = journal.New(cfg.Home, store, cfg.Barrier, nil)
	}
	// File API writes stage beside the content store, on the same disk.
	fs, err := files.New(cfg.Home, filepath.Join(contentDir, ".files"))
	if err != nil {
		return execenv.Error("agent", err)
	}
	if group != nil {
		fs.Own = group.OwnHome
		// A real guest watches its workspace, so an idle one is not walked.
		if w, err := workspace.New(cfg.Home); err == nil && s.journal != nil {
			s.journal.Watch(w)
			defer w.Close()
		}
	}
	s.files = fs
	if s.journal != nil {
		defer s.journal.Close()
	}
	// The process log must be recording before any workload process can
	// start. Without the kernel's process events the helper says so in
	// its handshake and the host refuses it.
	source := cfg.Processes
	if group != nil {
		source = nil
		if kernel, err := openKernelProcesses(group.Path(), cfg.Home, scratch); err == nil {
			source = kernel
		}
	}
	if source != nil {
		s.pumpProcesses(ctx, source)
	}
	defer s.shutdown()
	s.serveLink(ctx, conn, s)
	return nil
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
	_ = srv.Close()
	_ = link.Close()
	<-served
}

type server struct {
	home        string
	scratch     string
	workload    *workload.Group
	workloadErr error
	journal     *journal.Journal
	files       *files.FS
	// processes is whether the process log source is recording; events
	// carries its batches to the host's ProcessEvents stream.
	processes atomic.Bool
	events    chan []execenv.ProcessEvent

	mu     sync.Mutex
	frozen bool
	procs  map[string]*guestProc
}

func (s *server) shutdown() { s.stopAll() }

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

// ProcessEvents forwards the process log as it is recorded.
func (s *server) ProcessEvents(ctx context.Context, _ *connect.Request[guestv1.ProcessEventsRequest], stream *connect.ServerStream[guestv1.ProcessEventsResponse]) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case batch := <-s.events:
			page := wire.ProcessPageTo(execenv.ProcessPage{Events: batch})
			if err := stream.Send(&guestv1.ProcessEventsResponse{Events: page.GetEvents()}); err != nil {
				return err
			}
		}
	}
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
	if s.runningLocked() && !s.journal.Initialized() {
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

// ReadObservationContent streams a retained body from the content store.
// An unknown or retired one is refused before anything is sent.
func (s *server) ReadObservationContent(_ context.Context, req *connect.Request[execenvv1.ReadObservationContentRequest], stream *connect.ServerStream[execenvv1.ContentChunk]) error {
	ref := wire.ContentFrom(req.Msg.GetContent())
	if ref == nil || execenv.ValidateContentReference(*ref) != nil {
		return wire.Error(execenv.ErrInvalid)
	}
	s.mu.Lock()
	fence, err := s.bound(req.Msg.GetFence())
	var body *os.File
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
