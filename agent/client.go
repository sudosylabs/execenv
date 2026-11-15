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

	mu     sync.Mutex
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
	}
	go func() {
		<-link.done
		c.fail(execenv.ErrConnection)
	}()
	return c
}

// Verify rejects a helper that speaks another protocol, cannot serve the
// native journal, or is not recording the workload's processes. Host
// readiness must not infer capability from its own binary.
func (c *Client) Verify(ctx context.Context) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.Hello(ctx, connect.NewRequest(&guestv1.HelloRequest{}))
	if err != nil || resp.Msg.GetProtocol() != protocol || !resp.Msg.GetJournal() || !resp.Msg.GetProcesses() {
		return execenv.Error("agent protocol", execenv.ErrUnavailable)
	}
	return nil
}

// Done is closed once the guest link is gone, whether the guest end failed
// or Close hung it up. A client serves one link; a host that dials again
// uses a new client and finds the helper as this one left it.
func (c *Client) Done() <-chan struct{} { return c.link.done }

// Close hangs up the guest link. The machine is not torn down.
func (c *Client) Close() error {
	c.Hangup(execenv.ErrClosed)
	err := c.link.Close()
	c.transport.CloseIdleConnections()
	return err
}

// Hangup fails every process and call with err without closing the
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
	c.mu.Unlock()
	c.cancel()
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

// Reopen reopens I/O only after the guest acknowledges recovery.
func (c *Client) Reopen(ctx context.Context) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	if _, err := c.agent.Thaw(ctx, connect.NewRequest(&guestv1.ThawRequest{})); err != nil {
		return c.failure("thaw", err)
	}
	c.mu.Lock()
	c.frozen = false
	c.mu.Unlock()
	return nil
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
	if r.Fence != fence || r.MutationID != id || r.AppliedWorkspaceCursor != cursor || r.HostCursor != hostCursor || !wire.ValidDiscarded(r) {
		return execenv.ProjectionReceipt{}, execenv.ErrUnavailable
	}
	return r, nil
}

// Observe looks once: the guest never waits on the link, and a host that
// waits looks again itself.
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

// OpenObservationContent streams an observed file as it is now; a changed
// one ends with ErrStale. An unknown handle fails here, not at the first
// Read, so it cannot pass for an empty body.
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
