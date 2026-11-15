package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	guestv1 "github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1"
	"github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1/guestv1connect"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/processes"
	"github.com/sudosylabs/execenv/internal/wire"
)

// callTimeout bounds every unary call on the guest link.
const callTimeout = 30 * time.Second

// Client is the host side of one guest link.
type Client struct {
	link      *watchedConn
	transport *http.Transport
	agent     guestv1connect.AgentServiceClient
	ctx       context.Context
	cancel    context.CancelFunc
	// processes is the guest's process log, retained on the host until the
	// caller acknowledges it.
	processes *processes.Log

	mu     sync.Mutex
	term   *PTY
	frozen bool
	// hung is why the link was hung up by this side; it is every later
	// call's error.
	hung error
}

// NewClient takes an already-dialed guest link. It does not occupy a grant.
func NewClient(conn net.Conn) *Client {
	link := watch(conn)
	httpClient, transport := linkClient(link)
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		link:      link,
		transport: transport,
		agent:     guestv1connect.NewAgentServiceClient(httpClient, linkBase),
		ctx:       ctx,
		cancel:    cancel,
		processes: processes.New(),
	}
	go func() {
		<-link.done
		c.fail(execenv.ErrConnection)
	}()
	return c
}

// Verify rejects a helper that speaks another protocol, cannot serve the
// native journal, or is not recording the workload's processes. Host
// readiness must not infer capability from its own binary. Once verified,
// the process log flows to the host until the link ends.
func (c *Client) Verify(ctx context.Context) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.Hello(ctx, connect.NewRequest(&guestv1.HelloRequest{}))
	if err != nil || resp.Msg.GetProtocol() != protocol || !resp.Msg.GetJournal() || !resp.Msg.GetProcesses() {
		return execenv.Error("agent protocol", execenv.ErrUnavailable)
	}
	go c.receiveProcesses()
	return nil
}

func (c *Client) receiveProcesses() {
	stream, err := c.agent.ProcessEvents(c.ctx, connect.NewRequest(&guestv1.ProcessEventsRequest{}))
	if err != nil {
		return
	}
	defer stream.Close()
	for stream.Receive() {
		page := wire.ProcessPageFrom(&execenvv1.ProcessPage{Events: stream.Msg().GetEvents()})
		if len(page.Events) == 0 {
			c.processes.Lost(1)
			continue
		}
		for _, event := range page.Events {
			c.processes.Append(event)
		}
	}
}

// Done is closed once the guest link is gone, whether the guest end failed
// or Close hung it up. The link is never re-established: a new link would
// be a new helper session without this one's terminal or process log.
func (c *Client) Done() <-chan struct{} { return c.link.done }

// Close hangs up the guest link. The machine is not torn down.
func (c *Client) Close() error {
	c.Hangup(execenv.ErrClosed)
	err := c.link.Close()
	c.transport.CloseIdleConnections()
	return err
}

// Hangup fails the live PTY and every call with err without closing the
// connection. Revoke uses it so a killed grant surfaces ErrRevoked rather
// than a generic close.
func (c *Client) Hangup(err error) {
	c.mu.Lock()
	if c.hung == nil {
		c.hung = err
	}
	c.mu.Unlock()
	c.fail(err)
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.hung == nil {
		c.hung = execenv.ErrUnavailable
	}
	term := c.term
	c.term = nil
	c.mu.Unlock()
	c.cancel()
	if term != nil {
		term.hangup(err)
	}
}

func (c *Client) call(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	stop := context.AfterFunc(c.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// failure is a call's error: the hangup's cause once the link is hung up,
// else the guest's own error. A link that failed under the call is
// ErrUnavailable.
func (c *Client) failure(op string, err error) error {
	if err == nil {
		return nil
	}
	c.mu.Lock()
	hung := c.hung
	c.mu.Unlock()
	if hung != nil {
		return execenv.Error(op, hung)
	}
	mapped := wire.HostError(op, err)
	if errors.Is(mapped, execenv.ErrConnection) {
		return execenv.Error(op, execenv.ErrUnavailable)
	}
	return mapped
}

func (c *Client) ioGuard() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hung != nil {
		return c.hung
	}
	if c.frozen {
		return execenv.ErrFrozen
	}
	return nil
}

// Attach starts a login shell with cwd at Home. The guest ends that shell
// when the PTY is closed; the host keeps terminal sessions above this.
func (c *Client) Attach(ctx context.Context, win execenv.Window) (*PTY, error) {
	if err := c.ioGuard(); err != nil {
		return nil, execenv.Error("attach", err)
	}
	c.mu.Lock()
	if c.term != nil {
		c.mu.Unlock()
		return nil, execenv.Error("attach", execenv.ErrBusy)
	}
	t := &PTY{client: c}
	c.term = t
	c.mu.Unlock()
	release := func() {
		c.mu.Lock()
		if c.term == t {
			c.term = nil
		}
		c.mu.Unlock()
	}
	sctx, cancel := context.WithCancel(c.ctx)
	stream, err := c.agent.Attach(sctx, connect.NewRequest(&guestv1.AttachRequest{Window: wire.WindowTo(win)}))
	if err != nil {
		cancel()
		release()
		return nil, c.failure("attach", err)
	}
	started := make(chan error, 1)
	go func() {
		if !stream.Receive() {
			err := stream.Err()
			if err == nil {
				err = execenv.ErrUnavailable
			}
			started <- c.failure("attach", err)
			return
		}
		first := stream.Msg().GetStarted()
		if first == nil || first.GetShellId() == 0 {
			started <- execenv.Error("attach", execenv.ErrUnavailable)
			return
		}
		t.id = first.GetShellId()
		started <- nil
	}()
	select {
	case err = <-started:
	case <-ctx.Done():
		cancel()
		<-started
		err = execenv.Error("attach", ctx.Err())
	}
	if err != nil {
		cancel()
		release()
		return nil, err
	}
	t.stream, t.ctx, t.cancel = stream, sctx, cancel
	return t, nil
}

// Quiesce stops agent I/O. Home and the connection stay. The host calls it
// from fenced control before it pauses the machine.
func (c *Client) Quiesce(ctx context.Context) error {
	c.mu.Lock()
	c.frozen = true
	c.mu.Unlock()
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.agent.Freeze(ctx, connect.NewRequest(&guestv1.FreezeRequest{}))
	return c.failure("freeze", err)
}

// Reopen reopens I/O only after the guest acknowledges recovery. A PTY
// closed while frozen is detached now, before I/O reopens.
func (c *Client) Reopen(ctx context.Context) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	if _, err := c.agent.Thaw(ctx, connect.NewRequest(&guestv1.ThawRequest{})); err != nil {
		return c.failure("thaw", err)
	}
	c.mu.Lock()
	term := c.term
	c.mu.Unlock()
	if term != nil && term.isClosed() {
		if _, err := c.agent.Detach(ctx, connect.NewRequest(&guestv1.DetachRequest{ShellId: term.id})); err != nil {
			return c.failure("detach", err)
		}
		term.cancel()
		c.mu.Lock()
		if c.term == term {
			c.term = nil
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.frozen = false
	c.mu.Unlock()
	return nil
}

// PTY is the host end of the guest's one login shell. Read returns io.EOF
// once the shell exits and its output is drained, or after Close.
type PTY struct {
	client *Client
	id     uint64
	stream *connect.ServerStreamForClient[guestv1.AttachResponse]
	ctx    context.Context
	cancel context.CancelFunc

	readMu  sync.Mutex
	writeMu sync.Mutex
	input   *connect.ClientStreamForClient[guestv1.InputRequest, guestv1.InputResponse]

	mu      sync.Mutex
	pending []byte
	closed  bool
	ended   bool
	dead    error
}

func (t *PTY) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *PTY) hangup(err error) {
	t.mu.Lock()
	if t.dead == nil {
		if err == nil {
			err = execenv.ErrClosed
		}
		t.dead = err
	}
	t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *PTY) Read(p []byte) (int, error) {
	t.readMu.Lock()
	defer t.readMu.Unlock()
	for {
		t.mu.Lock()
		if len(t.pending) > 0 {
			n := copy(p, t.pending)
			t.pending = t.pending[n:]
			t.mu.Unlock()
			return n, nil
		}
		if t.dead != nil {
			err := t.dead
			t.mu.Unlock()
			return 0, execenv.Error("read", err)
		}
		if t.closed || t.ended {
			t.mu.Unlock()
			return 0, io.EOF
		}
		t.mu.Unlock()
		if !t.stream.Receive() {
			err := t.stream.Err()
			t.mu.Lock()
			switch {
			case t.dead != nil:
			case err == nil:
				t.ended = true
			case t.closed:
			default:
				t.dead = t.client.failure("read", err)
			}
			t.mu.Unlock()
			if err == nil {
				t.client.mu.Lock()
				if t.client.term == t {
					t.client.term = nil
				}
				t.client.mu.Unlock()
			}
			continue
		}
		out := t.stream.Msg().GetOutput()
		t.mu.Lock()
		t.pending = out
		t.mu.Unlock()
	}
}

func (t *PTY) Write(p []byte) (int, error) {
	if err := t.client.ioGuard(); err != nil {
		return 0, err
	}
	t.mu.Lock()
	if t.closed || t.ended || t.dead != nil {
		err := t.dead
		if err == nil {
			err = execenv.ErrClosed
		}
		t.mu.Unlock()
		return 0, execenv.Error("write", err)
	}
	t.mu.Unlock()
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.input == nil {
		input := t.client.agent.Input(t.ctx)
		if err := input.Send(&guestv1.InputRequest{Part: &guestv1.InputRequest_ShellId{ShellId: t.id}}); err != nil {
			_, _ = input.CloseAndReceive()
			return 0, execenv.Error("write", execenv.ErrClosed)
		}
		t.input = input
	}
	for rest := p; len(rest) > 0; {
		n := min(len(rest), wire.ChunkSize)
		if err := t.input.Send(&guestv1.InputRequest{Part: &guestv1.InputRequest_Data{Data: rest[:n]}}); err != nil {
			_, _ = t.input.CloseAndReceive()
			t.input = nil
			return len(p) - len(rest), execenv.Error("write", execenv.ErrClosed)
		}
		rest = rest[n:]
	}
	return len(p), nil
}

func (t *PTY) Resize(ctx context.Context, win execenv.Window) error {
	if err := t.client.ioGuard(); err != nil {
		return err
	}
	ctx, cancel := t.client.call(ctx)
	defer cancel()
	_, err := t.client.agent.Resize(ctx, connect.NewRequest(&guestv1.ResizeRequest{ShellId: t.id, Window: wire.WindowTo(win)}))
	return t.client.failure("resize", err)
}

// Close hangs the guest shell up. A paused guest cannot answer, so while
// frozen the shell stays attached internally and Reopen detaches it.
func (t *PTY) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.writeMu.Lock()
	if t.input != nil {
		_, _ = t.input.CloseAndReceive()
		t.input = nil
	}
	t.writeMu.Unlock()
	t.client.mu.Lock()
	frozen := t.client.frozen
	t.client.mu.Unlock()
	if frozen {
		return nil
	}
	ctx, cancel := t.client.call(context.Background())
	defer cancel()
	_, err := t.client.agent.Detach(ctx, connect.NewRequest(&guestv1.DetachRequest{ShellId: t.id}))
	t.cancel()
	t.client.mu.Lock()
	if t.client.term == t {
		t.client.term = nil
	}
	t.client.mu.Unlock()
	return t.client.failure("detach", err)
}

// UploadProjectionContent streams one body to the guest's journal.
func (c *Client) UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, body io.Reader) (execenv.ContentReference, error) {
	if body == nil || execenv.ValidateProjectionFence(fence) != nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	if err := c.ioGuard(); err != nil {
		return execenv.ContentReference{}, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	stream := c.agent.UploadContent(ctx)
	if err := stream.Send(&execenvv1.UploadContentRequest{Part: &execenvv1.UploadContentRequest_Fence{Fence: wire.FenceTo(fence)}}); err != nil {
		_, err := stream.CloseAndReceive()
		return execenv.ContentReference{}, c.failure("upload", err)
	}
	sum := sha256.New()
	var size int64
	buf := make([]byte, wire.ChunkSize)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			size += int64(n)
			_, _ = sum.Write(buf[:n])
			if serr := stream.Send(&execenvv1.UploadContentRequest{Part: &execenvv1.UploadContentRequest_Chunk{Chunk: buf[:n]}}); serr != nil {
				_, cerr := stream.CloseAndReceive()
				return execenv.ContentReference{}, c.failure("upload", cerr)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return execenv.ContentReference{}, err
		}
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return execenv.ContentReference{}, c.failure("upload", err)
	}
	ref := wire.ContentFrom(resp.Msg)
	if ref == nil || execenv.ValidateContentReference(*ref) != nil || ref.Size != size || ref.Digest != hex.EncodeToString(sum.Sum(nil)) {
		return execenv.ContentReference{}, execenv.ErrUnavailable
	}
	return *ref, nil
}

func (c *Client) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	if err := execenv.ValidateProjectionInitialization(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if err := c.ioGuard(); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.InitializeProjection(ctx, connect.NewRequest(wire.InitializationTo(request)))
	if err != nil {
		return execenv.ProjectionReceipt{}, c.failure("journal_initialize", err)
	}
	return checkReceipt(wire.ProjectionReceiptFrom(resp.Msg), request.Fence, request.MutationID, request.WorkspaceCursor, 0)
}

func (c *Client) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	if err := execenv.ValidateProjectionApply(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if err := c.ioGuard(); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.ApplyProjection(ctx, connect.NewRequest(wire.ApplyTo(request)))
	if err != nil {
		return execenv.ProjectionReceipt{}, c.failure("journal_apply", err)
	}
	return checkReceipt(wire.ProjectionReceiptFrom(resp.Msg), request.Fence, request.MutationID, request.ThroughWorkspaceCursor, request.ExpectedHostCursor)
}

func checkReceipt(r execenv.ProjectionReceipt, fence execenv.ProjectionFence, id string, cursor, hostCursor uint64) (execenv.ProjectionReceipt, error) {
	if r.Fence != fence || r.MutationID != id || r.AppliedWorkspaceCursor != cursor || r.HostCursor != hostCursor {
		return execenv.ProjectionReceipt{}, execenv.ErrUnavailable
	}
	return r, nil
}

func (c *Client) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	if err := c.ioGuard(); err != nil {
		return execenv.ObservationPage{}, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.Observe(ctx, connect.NewRequest(&execenvv1.ObserveRequest{Fence: wire.FenceTo(fence), After: after, Limit: int32(limit)}))
	if err != nil {
		return execenv.ObservationPage{}, c.failure("journal_observe", err)
	}
	page := wire.ObservationPageFrom(resp.Msg)
	if page.HostCursor > execenv.MaxProjectionCursor || page.HostCursor < after || page.AcknowledgedHostCursor > after || len(page.Observations) > limit {
		return execenv.ObservationPage{}, execenv.ErrUnavailable
	}
	for i, event := range page.Observations {
		if execenv.ValidateExecutionObservation(event) != nil || event.Fence.ExecutionGrantID != fence.ExecutionGrantID || event.Fence.EnvironmentEpoch != fence.EnvironmentEpoch || event.HostSequence != after+uint64(i)+1 || event.HostSequence > page.HostCursor {
			return execenv.ObservationPage{}, execenv.ErrUnavailable
		}
	}
	return page, nil
}

func (c *Client) AcknowledgeObservations(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	if err := c.ioGuard(); err != nil {
		return err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.agent.AcknowledgeObservations(ctx, connect.NewRequest(&execenvv1.AcknowledgeObservationsRequest{Fence: wire.FenceTo(fence), Through: through}))
	return c.failure("journal_acknowledge", err)
}

// OpenObservationContent streams a retained body. An unknown handle fails
// here, not at the first Read, so it cannot pass for an empty body.
func (c *Client) OpenObservationContent(ctx context.Context, fence execenv.ProjectionFence, ref execenv.ContentReference) (io.ReadCloser, error) {
	if execenv.ValidateContentReference(ref) != nil {
		return nil, execenv.ErrInvalid
	}
	if err := c.ioGuard(); err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	stream, err := c.agent.ReadObservationContent(sctx, connect.NewRequest(&execenvv1.ReadObservationContentRequest{Fence: wire.FenceTo(fence), Content: wire.ContentTo(&ref)}))
	if err != nil {
		stop()
		cancel()
		return nil, c.failure("journal_content", err)
	}
	r := wire.NewContentReader(stream, ref, "journal_content", func() { stop(); cancel() })
	r.Fail = func(err error) error { return c.failure("journal_content", err) }
	if err := r.Prime(); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (c *Client) ConfirmObservationProjection(ctx context.Context, binding execenv.ObservationProjection) error {
	if err := execenv.ValidateObservationProjection(binding); err != nil {
		return err
	}
	if err := c.ioGuard(); err != nil {
		return err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.agent.ConfirmObservationProjection(ctx, connect.NewRequest(&execenvv1.ConfirmObservationProjectionRequest{
		Fence:        wire.FenceTo(binding.Fence),
		HostSequence: binding.HostSequence,
		Mutation:     wire.MutationTo(binding.Mutation),
	}))
	return c.failure("journal_confirm", err)
}
