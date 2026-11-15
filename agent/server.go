package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	guestv1 "github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1"
	"github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1/guestv1connect"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
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
type WriteBarrier interface {
	Stopped(ctx context.Context, fn func() error) error
}

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
	}
	switch {
	case group != nil:
		s.journal = newDiskJournal(cfg.Home, group, group.OwnHome)
	case cfg.Barrier != nil:
		s.journal = newDiskJournal(cfg.Home, cfg.Barrier, nil)
	}
	if s.journal != nil {
		defer s.journal.close()
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
	journal     *diskJournal
	// processes is whether the process log source is recording; events
	// carries its batches to the host's ProcessEvents stream.
	processes atomic.Bool
	events    chan []execenv.ProcessEvent

	mu     sync.Mutex
	frozen bool
	term   *shell
	termID uint64
	nextID uint64
}

func (s *server) shutdown() {
	s.mu.Lock()
	s.killShell()
	s.mu.Unlock()
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

// Attach starts the login shell and streams its output. The shell ends with
// Detach, with the stream, or with the link.
func (s *server) Attach(ctx context.Context, req *connect.Request[guestv1.AttachRequest], stream *connect.ServerStream[guestv1.AttachResponse]) error {
	window, ok := wire.WindowFrom(req.Msg.GetWindow())
	if !ok {
		return wire.Error(execenv.ErrInvalid)
	}
	s.mu.Lock()
	if err := s.guard(); err != nil {
		s.mu.Unlock()
		return wire.Error(err)
	}
	if s.term != nil && !s.term.done() {
		s.mu.Unlock()
		return wire.Error(execenv.ErrBusy)
	}
	if s.workload != nil {
		if err := s.workload.Kill(ctx); err != nil {
			s.workloadErr = err
			s.mu.Unlock()
			return wire.Error(err)
		}
	}
	sh, err := startShell(s.home, s.scratch, window, s.workload)
	if err != nil {
		s.mu.Unlock()
		return wire.Error(execenv.ErrUnavailable)
	}
	s.nextID++
	id := s.nextID
	s.term, s.termID = sh, id
	s.mu.Unlock()
	// A stream the host abandons takes its shell with it, unless the guest
	// is frozen: then the host detaches once it has thawed.
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		if s.termID == id && !s.frozen {
			s.killShell()
		}
		s.mu.Unlock()
	})
	defer stop()

	if err := stream.Send(&guestv1.AttachResponse{Event: &guestv1.AttachResponse_Started{Started: &guestv1.Started{ShellId: id}}}); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := sh.master.Read(buf)
		if n > 0 {
			if serr := stream.Send(&guestv1.AttachResponse{Event: &guestv1.AttachResponse_Output{Output: buf[:n]}}); serr != nil {
				return serr
			}
		}
		if err != nil {
			s.mu.Lock()
			frozen := s.frozen
			if s.term == sh {
				s.term, s.termID = nil, 0
			}
			s.mu.Unlock()
			return wire.Error(ptyEnd(frozen, err))
		}
	}
}

// ptyEnd is why a shell's output ended: nil for its exit or hangup. A
// guest frozen under it reports that instead, so the host keeps the shell.
func ptyEnd(frozen bool, err error) error {
	switch {
	case frozen:
		return execenv.ErrFrozen
	case err == nil || err == io.EOF || isPtyHangup(err):
		return nil
	default:
		return err
	}
}

func (s *server) current(id uint64) (*shell, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term, s.term != nil && s.termID == id && !s.frozen
}

// Input writes keystrokes to the shell it names. Input for a shell that has
// gone, or arriving while frozen, is dropped.
func (s *server) Input(_ context.Context, stream *connect.ClientStream[guestv1.InputRequest]) (*connect.Response[guestv1.InputResponse], error) {
	if !stream.Receive() {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	id := stream.Msg().GetShellId()
	for stream.Receive() {
		data := stream.Msg().GetData()
		if sh, ok := s.current(id); ok {
			_, _ = sh.master.Write(data)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&guestv1.InputResponse{}), nil
}

func (s *server) Resize(_ context.Context, req *connect.Request[guestv1.ResizeRequest]) (*connect.Response[guestv1.ResizeResponse], error) {
	window, ok := wire.WindowFrom(req.Msg.GetWindow())
	if !ok {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	s.mu.Lock()
	term, id, frozen := s.term, s.termID, s.frozen
	s.mu.Unlock()
	if frozen {
		return nil, wire.Error(execenv.ErrFrozen)
	}
	if term == nil || id != req.Msg.GetShellId() {
		return nil, wire.Error(execenv.ErrClosed)
	}
	if err := setWindow(term.master, window); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&guestv1.ResizeResponse{}), nil
}

func (s *server) Detach(_ context.Context, req *connect.Request[guestv1.DetachRequest]) (*connect.Response[guestv1.DetachResponse], error) {
	s.mu.Lock()
	if s.termID == req.Msg.GetShellId() {
		s.killShell()
	}
	s.mu.Unlock()
	return connect.NewResponse(&guestv1.DetachResponse{}), nil
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
	var data []byte
	for stream.Receive() {
		chunk := stream.Msg().GetChunk()
		if len(data)+len(chunk) > execenv.MaxFileBytes {
			return nil, wire.Error(execenv.ErrTooLarge)
		}
		data = append(data, chunk...)
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.journalGuard(); err != nil {
		return nil, wire.Error(err)
	}
	if err := s.journal.bind(fence); err != nil {
		return nil, wire.Error(err)
	}
	ref, err := s.journal.upload(fence, data)
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
	if s.term != nil && !s.journal.state.Initialized() {
		return nil, wire.Error(execenv.ErrConflict)
	}
	receipt, err := s.journal.initialize(ctx, wire.InitializationFrom(req.Msg))
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
	receipt, err := s.journal.apply(ctx, wire.ApplyFrom(req.Msg))
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
	return f, s.journal.bind(f)
}

func (s *server) Observe(ctx context.Context, req *connect.Request[execenvv1.ObserveRequest]) (*connect.Response[execenvv1.ObservationPage], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fence, err := s.bound(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	page, err := s.journal.observe(ctx, fence, req.Msg.GetAfter(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ObservationPageTo(page)), nil
}

// ReadObservationContent streams a retained body. An unknown or retired one
// is refused before anything is sent.
func (s *server) ReadObservationContent(_ context.Context, req *connect.Request[execenvv1.ReadObservationContentRequest], stream *connect.ServerStream[execenvv1.ContentChunk]) error {
	ref := wire.ContentFrom(req.Msg.GetContent())
	if ref == nil || execenv.ValidateContentReference(*ref) != nil {
		return wire.Error(execenv.ErrInvalid)
	}
	chunk := func(offset int64) ([]byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, err := s.bound(req.Msg.GetFence()); err != nil {
			return nil, err
		}
		return s.journal.log.Chunk(*ref, offset, wire.ChunkSize)
	}
	var offset int64
	for {
		body, err := chunk(offset)
		if err != nil {
			return wire.Error(err)
		}
		if len(body) == 0 {
			return nil
		}
		if err := stream.Send(&execenvv1.ContentChunk{Data: body}); err != nil {
			return err
		}
		offset += int64(len(body))
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
	if err := s.journal.confirm(binding); err != nil {
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
	if err := s.journal.acknowledge(fence, req.Msg.GetThrough()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.AcknowledgeObservationsResponse{}), nil
}

func (s *server) killShell() {
	if s.term == nil {
		return
	}
	s.term.close()
	if s.workload != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.workloadErr = s.workload.Kill(ctx)
		cancel()
	}
	s.term, s.termID = nil, 0
}

type shell struct {
	master *os.File
	cmd    *exec.Cmd
	exited chan struct{}
}

func (sh *shell) done() bool {
	if sh == nil {
		return true
	}
	select {
	case <-sh.exited:
		return true
	default:
		return false
	}
}

func (sh *shell) close() {
	if sh.cmd.Process != nil {
		hangup(sh.cmd, sh.exited)
	}
	_ = sh.master.Close()
}
