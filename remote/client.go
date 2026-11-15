package remote

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/gen/execenv/v1/execenvv1connect"
	"github.com/sudosylabs/execenv/internal/wire"
)

var _ execenv.Host = (*Client)(nil)

// Window sizes for HTTP/2 flow control on the client. The interactive
// connection keeps terminal streams small so one busy shell cannot starve
// another; the bulk connection carries file content.
const (
	interactiveStreamWindow = 256 << 10
	bulkStreamWindow        = 4 << 20
	clientConnectionWindow  = 16 << 20
)

// Client is a Host served over the network. It keeps two HTTP/2
// connections to the host: one for control, terminals and the logs, one
// for file content, so a large upload never delays a keystroke.
type Client struct {
	cfg          Config
	capabilities execenv.Capabilities
	ctx          context.Context
	cancel       context.CancelFunc
	transports   []*http.Transport

	host    execenvv1connect.HostServiceClient
	control execenvv1connect.ControlServiceClient
	journal execenvv1connect.JournalServiceClient
	content execenvv1connect.JournalServiceClient
	// bulkHost carries seed archives on the content connection.
	bulkHost execenvv1connect.HostServiceClient
	// files carries file metadata; bulkFiles carries file bodies.
	files     execenvv1connect.FileServiceClient
	bulkFiles execenvv1connect.FileServiceClient
	terminal  execenvv1connect.TerminalServiceClient
	process   execenvv1connect.ProcessLogServiceClient
}

// New connects to cfg.Address and says hello. It occupies no grant. It
// fails with ErrFeature when the host does not offer what this client
// needs, and with ErrUnavailable when the host refuses its credentials.
func New(cfg Config) (*Client, error) {
	if err := validateClient(cfg); err != nil {
		return nil, err
	}
	interactive, interactiveTransport := httpClient(cfg, interactiveStreamWindow)
	bulk, bulkTransport := httpClient(cfg, bulkStreamWindow)
	base := "http://" + cfg.Address
	if cfg.Security == SecurityTLS {
		base = "https://" + cfg.Address
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		cfg:        cfg,
		ctx:        ctx,
		cancel:     cancel,
		transports: []*http.Transport{interactiveTransport, bulkTransport},
		host:       execenvv1connect.NewHostServiceClient(interactive, base),
		control:    execenvv1connect.NewControlServiceClient(interactive, base),
		journal:    execenvv1connect.NewJournalServiceClient(interactive, base),
		content:    execenvv1connect.NewJournalServiceClient(bulk, base),
		bulkHost:   execenvv1connect.NewHostServiceClient(bulk, base),
		files:      execenvv1connect.NewFileServiceClient(interactive, base),
		bulkFiles:  execenvv1connect.NewFileServiceClient(bulk, base),
		terminal:   execenvv1connect.NewTerminalServiceClient(interactive, base),
		process:    execenvv1connect.NewProcessLogServiceClient(interactive, base),
	}
	hctx, hcancel := context.WithTimeout(ctx, timeoutOrDefault(cfg.Timeout))
	defer hcancel()
	hello, err := c.host.Hello(hctx, connect.NewRequest(&execenvv1.HelloRequest{RequiredFeatures: features}))
	if err != nil {
		_ = c.Close()
		return nil, wire.HostError("dial", err)
	}
	if hello.Msg.GetProtocol() != protocol {
		_ = c.Close()
		return nil, execenv.Error("dial", execenv.ErrFeature)
	}
	c.capabilities = execenv.Capabilities{Isolated: hello.Msg.GetIsolated()}
	return c, nil
}

func httpClient(cfg Config, streamWindow int) (*http.Client, *http.Transport) {
	protocols := new(http.Protocols)
	dialer := &net.Dialer{Timeout: timeoutOrDefault(cfg.Timeout), KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:     dialer.DialContext,
		IdleConnTimeout: 0,
		HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerStream:     streamWindow,
			MaxReceiveBufferPerConnection: clientConnectionWindow,
			SendPingTimeout:               30 * time.Second,
			PingTimeout:                   15 * time.Second,
		},
	}
	if cfg.Security == SecurityTLS {
		protocols.SetHTTP2(true)
		tlsCfg := cfg.TLS.Clone()
		if tlsCfg.MinVersion == 0 {
			tlsCfg.MinVersion = tls.VersionTLS13
		}
		if tlsCfg.ServerName == "" {
			tlsCfg.ServerName = cfg.ServerName
		}
		transport.TLSClientConfig = tlsCfg
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}
	transport.Protocols = protocols
	var rt http.RoundTripper = transport
	if len(cfg.Token) > 0 {
		rt = bearer{token: "Bearer " + string(cfg.Token), next: transport}
	}
	return &http.Client{Transport: rt}, transport
}

// bearer adds the token to every request. It never logs it.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", b.token)
	return b.next.RoundTrip(r)
}

// Close ends every stream and connection. Live grants on the host stay;
// their terminals detach.
func (c *Client) Close() error {
	c.cancel()
	for _, t := range c.transports {
		t.CloseIdleConnections()
	}
	return nil
}

// call bounds one unary call by the client's operation timeout and by
// Close.
func (c *Client) call(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeoutOrDefault(c.cfg.OperationTimeout))
	stop := context.AfterFunc(c.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// Capabilities is what the host reported in Hello.
func (c *Client) Capabilities() execenv.Capabilities { return c.capabilities }

func (c *Client) Ready(ctx context.Context) (execenv.Report, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.host.GetReport(ctx, connect.NewRequest(&execenvv1.GetReportRequest{}))
	if err != nil {
		return execenv.Report{}, wire.HostError("ready", err)
	}
	return wire.ReportFrom(resp.Msg), nil
}

func (c *Client) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	if err := execenv.ValidateSpec(spec); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.host.EnsureGrant(ctx, connect.NewRequest(&execenvv1.EnsureGrantRequest{Spec: wire.SpecTo(spec)}))
	if err != nil {
		return nil, wire.HostError("ensure", err)
	}
	if resp.Msg.GetEpoch() == "" {
		return nil, execenv.Error("ensure", execenv.ErrUnavailable)
	}
	return &remoteEnv{client: c, id: spec.ID, epoch: resp.Msg.GetEpoch()}, nil
}

func (c *Client) Revoke(ctx context.Context, id execenv.ID) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.host.RevokeGrant(ctx, connect.NewRequest(&execenvv1.RevokeGrantRequest{GrantId: string(id)}))
	return wire.HostError("revoke", err)
}

type remoteEnv struct {
	client  *Client
	id      execenv.ID
	epoch   string
	revoked atomic.Bool

	mu    sync.Mutex
	terms map[*remoteTerminal]struct{}
}

func (e *remoteEnv) ID() execenv.ID { return e.id }
func (e *remoteEnv) Epoch() string  { return e.epoch }
func (e *remoteEnv) alive() error {
	if e.revoked.Load() {
		return execenv.ErrRevoked
	}
	return nil
}

func (e *remoteEnv) guard(fence execenv.ProjectionFence) error {
	if err := e.alive(); err != nil {
		return err
	}
	if fence.ExecutionGrantID != e.id || fence.EnvironmentEpoch != e.epoch {
		return execenv.ErrConflict
	}
	return execenv.ValidateProjectionFence(fence)
}

func (e *remoteEnv) Revoke(ctx context.Context) error {
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.host.RevokeGrant(ctx, connect.NewRequest(&execenvv1.RevokeGrantRequest{GrantId: string(e.id), Epoch: e.epoch}))
	if err != nil {
		return wire.HostError("revoke", err)
	}
	e.revoked.Store(true)
	e.endTerminals(execenv.ErrRevoked)
	return nil
}

func (e *remoteEnv) Control(ctx context.Context, request execenv.ControlRequest) (execenv.ControlReceipt, error) {
	if request.Fence.ExecutionGrantID != e.id || request.Fence.EnvironmentEpoch != e.epoch {
		return execenv.ControlReceipt{}, execenv.ErrConflict
	}
	if err := execenv.ValidateControl(request); err != nil {
		return execenv.ControlReceipt{}, err
	}
	if request.State != execenv.ControlRunning {
		e.setFrozen(true)
	}
	cctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.control.Control(cctx, connect.NewRequest(&execenvv1.ControlRequest{Fence: wire.FenceTo(request.Fence), State: wire.ControlStateTo(request.State)}))
	if err != nil {
		return execenv.ControlReceipt{}, wire.HostError("control", err)
	}
	receipt := wire.ReceiptFrom(resp.Msg)
	if receipt.Fence != request.Fence || receipt.State != request.State || !receipt.Confirmed {
		return execenv.ControlReceipt{}, execenv.ErrUnavailable
	}
	if request.State == execenv.ControlRevoked {
		e.revoked.Store(true)
		e.endTerminals(execenv.ErrRevoked)
	} else {
		e.setFrozen(request.State == execenv.ControlFrozen)
	}
	return receipt, nil
}

func (e *remoteEnv) ControlStatus(ctx context.Context) (execenv.ControlReceipt, error) {
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.control.ControlStatus(ctx, connect.NewRequest(&execenvv1.ControlStatusRequest{GrantId: string(e.id), Epoch: e.epoch}))
	if err != nil {
		return execenv.ControlReceipt{}, wire.HostError("control_status", err)
	}
	receipt := wire.ReceiptFrom(resp.Msg)
	if receipt.Fence.ExecutionGrantID != e.id || receipt.Fence.EnvironmentEpoch != e.epoch {
		return execenv.ControlReceipt{}, execenv.ErrUnavailable
	}
	return receipt, nil
}

func (e *remoteEnv) UpdateLimits(ctx context.Context, fence execenv.ProjectionFence, limits execenv.Limits) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	if err := execenv.ValidateLimits(limits); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.control.UpdateLimits(ctx, connect.NewRequest(&execenvv1.UpdateLimitsRequest{Fence: wire.FenceTo(fence), Limits: wire.LimitsTo(limits)}))
	return wire.HostError("update_limits", err)
}

// UploadProjectionContent streams body on the bulk connection. The host
// hashes what it receives; a reference that does not match what was sent
// is refused here.
func (e *remoteEnv) UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, body io.Reader) (execenv.ContentReference, error) {
	if err := e.guard(fence); err != nil {
		return execenv.ContentReference{}, err
	}
	if body == nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(e.client.ctx, cancel)
	defer stop()
	stream := e.client.content.UploadContent(ctx)
	if err := stream.Send(&execenvv1.UploadContentRequest{Part: &execenvv1.UploadContentRequest_Fence{Fence: wire.FenceTo(fence)}}); err != nil {
		return execenv.ContentReference{}, uploadError(stream)
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
				return execenv.ContentReference{}, uploadError(stream)
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
		return execenv.ContentReference{}, wire.HostError("upload", err)
	}
	ref := wire.ContentFrom(resp.Msg)
	if ref == nil || execenv.ValidateContentReference(*ref) != nil || ref.Size != size || ref.Digest != hex.EncodeToString(sum.Sum(nil)) {
		return execenv.ContentReference{}, execenv.ErrUnavailable
	}
	return *ref, nil
}

func uploadError(stream *connect.ClientStreamForClient[execenvv1.UploadContentRequest, execenvv1.ContentReference]) error {
	_, err := stream.CloseAndReceive()
	if err == nil {
		err = execenv.ErrUnavailable
	}
	return wire.HostError("upload", err)
}

func (e *remoteEnv) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	if err := e.guard(request.Fence); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if err := execenv.ValidateProjectionInitialization(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.journal.InitializeProjection(ctx, connect.NewRequest(wire.InitializationTo(request)))
	if err != nil {
		return execenv.ProjectionReceipt{}, wire.HostError("projection_initialize", err)
	}
	return e.receipt(wire.ProjectionReceiptFrom(resp.Msg), request.Fence, request.MutationID)
}

func (e *remoteEnv) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	if err := e.guard(request.Fence); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if err := execenv.ValidateProjectionApply(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.journal.ApplyProjection(ctx, connect.NewRequest(wire.ApplyTo(request)))
	if err != nil {
		return execenv.ProjectionReceipt{}, wire.HostError("projection_apply", err)
	}
	return e.receipt(wire.ProjectionReceiptFrom(resp.Msg), request.Fence, request.MutationID)
}

func (e *remoteEnv) receipt(r execenv.ProjectionReceipt, fence execenv.ProjectionFence, mutation string) (execenv.ProjectionReceipt, error) {
	if r.Fence != fence || r.MutationID != mutation {
		return execenv.ProjectionReceipt{}, execenv.ErrUnavailable
	}
	return r, nil
}

func (e *remoteEnv) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	if err := e.guard(fence); err != nil {
		return execenv.ObservationPage{}, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.journal.Observe(ctx, connect.NewRequest(&execenvv1.ObserveRequest{Fence: wire.FenceTo(fence), After: after, Limit: int32(limit)}))
	if err != nil {
		return execenv.ObservationPage{}, wire.HostError("observe", err)
	}
	page := wire.ObservationPageFrom(resp.Msg)
	for _, o := range page.Observations {
		if execenv.ValidateExecutionObservation(o) != nil || o.Fence.ExecutionGrantID != e.id || o.Fence.EnvironmentEpoch != e.epoch {
			return execenv.ObservationPage{}, execenv.ErrUnavailable
		}
	}
	return page, nil
}

// OpenObservationContent streams the body on the bulk connection. The
// reader checks size and digest at its end, so a body that does not match
// its reference fails instead of ending cleanly.
func (e *remoteEnv) OpenObservationContent(ctx context.Context, fence execenv.ProjectionFence, ref execenv.ContentReference) (io.ReadCloser, error) {
	if err := e.guard(fence); err != nil {
		return nil, err
	}
	if err := execenv.ValidateContentReference(ref); err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.client.ctx, cancel)
	stream, err := e.client.content.ReadObservationContent(sctx, connect.NewRequest(&execenvv1.ReadObservationContentRequest{Fence: wire.FenceTo(fence), Content: wire.ContentTo(&ref)}))
	if err != nil {
		stop()
		cancel()
		return nil, wire.HostError("observation_content", err)
	}
	r := wire.NewContentReader(stream, ref, "observation_content", func() { stop(); cancel() })
	if err := r.Prime(); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (e *remoteEnv) ConfirmObservationProjection(ctx context.Context, binding execenv.ObservationProjection) error {
	if err := e.guard(binding.Fence); err != nil {
		return err
	}
	if err := execenv.ValidateObservationProjection(binding); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.journal.ConfirmObservationProjection(ctx, connect.NewRequest(&execenvv1.ConfirmObservationProjectionRequest{
		Fence:        wire.FenceTo(binding.Fence),
		HostSequence: binding.HostSequence,
		Mutation:     wire.MutationTo(binding.Mutation),
	}))
	return wire.HostError("observation_confirm", err)
}

func (e *remoteEnv) AcknowledgeObservations(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.journal.AcknowledgeObservations(ctx, connect.NewRequest(&execenvv1.AcknowledgeObservationsRequest{Fence: wire.FenceTo(fence), Through: through}))
	return wire.HostError("observation_acknowledge", err)
}

func (e *remoteEnv) ObserveProcesses(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ProcessPage, error) {
	if err := e.guard(fence); err != nil {
		return execenv.ProcessPage{}, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.process.ObserveProcesses(ctx, connect.NewRequest(&execenvv1.ObserveProcessesRequest{Fence: wire.FenceTo(fence), After: after, Limit: int32(limit)}))
	if err != nil {
		return execenv.ProcessPage{}, wire.HostError("process_observe", err)
	}
	page := wire.ProcessPageFrom(resp.Msg)
	for _, event := range page.Events {
		if execenv.ValidateProcessEvent(event) != nil {
			return execenv.ProcessPage{}, execenv.ErrUnavailable
		}
	}
	return page, nil
}

func (e *remoteEnv) AcknowledgeProcesses(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.process.AcknowledgeProcesses(ctx, connect.NewRequest(&execenvv1.AcknowledgeProcessesRequest{Fence: wire.FenceTo(fence), Through: through}))
	return wire.HostError("process_acknowledge", err)
}
