package remote

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/gen/execenv/v1/execenvv1connect"
	"github.com/sudosylabs/execenv/internal/wire"
)

// protocol is the major version of the schema in proto/execenv/v1.
const protocol = 3

// Features this host offers. A client names the ones it needs in Hello.
const (
	FeatureControl    = "control"
	FeatureJournal    = "journal"
	FeatureProcessLog = "process_log"
	// FeatureObserveWait is a host whose Observe waits for a change.
	FeatureObserveWait = "observe_wait"
	// FeatureObservationTag is a host that names, on each observation, the
	// started process the change's writer descends from, where it can.
	FeatureObservationTag = "observation_tag"
	// FeatureOverwrite is a host whose projection can overwrite the
	// guest's unconfirmed changes and discard their observations.
	FeatureOverwrite       = "projection_overwrite"
	FeaturePortConnections = "port_connections"
)

var features = []string{FeaturePortConnections, FeatureControl, FeatureJournal, FeatureProcess, FeatureProcessLog, FeatureSeed, FeatureFiles, FeatureScreen, FeatureGrants, FeatureObserveWait, FeatureObservationTag, FeatureOverwrite}

// Window sizes for HTTP/2 flow control. The host receives uploads and
// keystrokes; uploads need the large window.
const (
	hostStreamWindow     = 4 << 20
	hostConnectionWindow = 16 << 20
)

// Serve accepts connections on ln and serves inner until ctx ends. Ending an
// attachment's stream, or losing the connection that carries it, detaches
// it: the process keeps running on inner, and grants stay.
func Serve(ctx context.Context, ln net.Listener, inner execenv.Host, cfg ServerConfig) error {
	if cfg.Security == SecurityTLS && cfg.TLS != nil {
		cfg.TLS = cfg.TLS.Clone()
		if cfg.TLS.MinVersion == 0 {
			cfg.TLS.MinVersion = tls.VersionTLS13
		}
	}
	if err := validateServer(cfg); err != nil {
		return err
	}
	if inner == nil || ln == nil {
		return execenv.Error("serve", execenv.ErrInvalid)
	}
	switch cfg.Security {
	case SecurityTLS:
		if err := execenv.RequireIsolated(inner); err != nil {
			return err
		}
	case SecurityInsecureLocal:
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok || !addr.IP.IsLoopback() {
			return execenv.Error("serve", execenv.ErrInvalid)
		}
	}
	h := &server{inner: inner, cfg: cfg, envs: make(map[execenv.ID]execenv.Env)}
	mux := http.NewServeMux()
	options := connect.WithInterceptors(timeoutInterceptor{operationTimeoutOrDefault(cfg.OperationTimeout)})
	mux.Handle(execenvv1connect.NewHostServiceHandler(h, options))
	mux.Handle(execenvv1connect.NewControlServiceHandler(h, options))
	mux.Handle(execenvv1connect.NewJournalServiceHandler(h, options))
	mux.Handle(execenvv1connect.NewProcessServiceHandler(h, options))
	mux.Handle(execenvv1connect.NewPortServiceHandler(h, options, connect.WithReadMaxBytes(wire.MaxPortMessage)))
	mux.Handle(execenvv1connect.NewProcessLogServiceHandler(h, options))
	mux.Handle(execenvv1connect.NewFileServiceHandler(h, options))

	protocols := new(http.Protocols)
	if cfg.Security == SecurityTLS {
		protocols.SetHTTP1(true)
		protocols.SetHTTP2(true)
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}
	srv := &http.Server{
		Handler:           h.authenticate(mux),
		Protocols:         protocols,
		TLSConfig:         cfg.TLS,
		ReadHeaderTimeout: timeoutOrDefault(cfg.AuthTimeout),
		HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerStream:     hostStreamWindow,
			MaxReceiveBufferPerConnection: hostConnectionWindow,
			SendPingTimeout:               30 * time.Second,
		},
		ErrorLog: discardLog,
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
		case <-done:
		}
	}()
	var err error
	if cfg.Security == SecurityTLS {
		err = srv.ServeTLS(ln, "", "")
	} else {
		err = srv.Serve(ln)
	}
	if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return execenv.Error("serve", err)
}

type server struct {
	inner execenv.Host
	cfg   ServerConfig

	mu sync.Mutex
	// envs holds the environment each grant was last ensured as.
	envs map[execenv.ID]execenv.Env
	// lifecycle serializes the adapter call and map publication for one ID.
	lifecycle map[execenv.ID]*grantOperation
}

type grantOperation struct {
	gate         chan struct{}
	refs         int
	cancelEnsure context.CancelFunc
	revokes      int
}

func (h *server) lockGrant(ctx context.Context, id execenv.ID, ensure context.CancelFunc, revoke bool) (func(), error) {
	h.mu.Lock()
	if h.lifecycle == nil {
		h.lifecycle = make(map[execenv.ID]*grantOperation)
	}
	op := h.lifecycle[id]
	if op == nil {
		op = &grantOperation{gate: make(chan struct{}, 1)}
		h.lifecycle[id] = op
	}
	op.refs++
	if revoke {
		op.revokes++
		if op.cancelEnsure != nil {
			op.cancelEnsure()
		}
	}
	h.mu.Unlock()
	drop := func(held bool) {
		h.mu.Lock()
		if held && ensure != nil {
			op.cancelEnsure = nil
		}
		if revoke {
			op.revokes--
		}
		op.refs--
		if op.refs == 0 {
			delete(h.lifecycle, id)
		}
		h.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop(false)
		return nil, ctx.Err()
	case op.gate <- struct{}{}:
		h.mu.Lock()
		if ensure != nil {
			op.cancelEnsure = ensure
			if op.revokes > 0 {
				ensure()
			}
		}
		h.mu.Unlock()
		unlock := func() {
			drop(true)
			<-op.gate
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	}
}

// authenticate admits a request that carries the token or a verified
// client certificate. Comparison is constant time; the token never leaves
// this function.
func (h *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *server) authorized(r *http.Request) bool {
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		return true
	}
	if len(h.cfg.Token) == 0 {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), h.cfg.Token) == 1
}

// timeoutInterceptor caps every unary call. Streams live as long as their
// client keeps them.
type timeoutInterceptor struct{ limit time.Duration }

func (t timeoutInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		limit := t.limit
		// An observation wait is not the operation: it is added to the cap.
		if observe, ok := req.Any().(*execenvv1.ObserveRequest); ok && observe.GetWait() != nil {
			limit += min(max(observe.GetWait().AsDuration(), 0), execenv.MaxObserveWait)
		}
		ctx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		return next(ctx, req)
	}
}

func (timeoutInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (timeoutInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// env is the grant's current environment, refused unless epoch names it.
func (h *server) env(id execenv.ID, epoch string) (execenv.Env, error) {
	h.mu.Lock()
	env, ok := h.envs[id]
	h.mu.Unlock()
	if !ok {
		return nil, execenv.ErrRevoked
	}
	if env.Epoch() != epoch {
		return nil, execenv.ErrConflict
	}
	return env, nil
}

func (h *server) fenced(fence *execenvv1.Fence) (execenv.Env, execenv.ProjectionFence, error) {
	f := wire.FenceFrom(fence)
	env, err := h.env(f.ExecutionGrantID, f.EnvironmentEpoch)
	return env, f, err
}

func (h *server) forget(id execenv.ID) {
	h.mu.Lock()
	delete(h.envs, id)
	h.mu.Unlock()
}

// HostService

func (h *server) Hello(_ context.Context, req *connect.Request[execenvv1.HelloRequest]) (*connect.Response[execenvv1.HelloResponse], error) {
	for _, want := range req.Msg.GetRequiredFeatures() {
		if !offered(want) {
			return nil, wire.WithDetail(connect.NewError(connect.CodeFailedPrecondition, execenv.ErrFeature), &execenvv1.ErrorDetail{Sentinel: "feature", Feature: want})
		}
	}
	return connect.NewResponse(&execenvv1.HelloResponse{
		Protocol: protocol,
		Features: features,
		Isolated: h.inner.Capabilities().Isolated,
		Release:  execenv.Release,
	}), nil
}

func offered(feature string) bool {
	for _, f := range features {
		if f == feature {
			return true
		}
	}
	return false
}

func (h *server) GetReport(ctx context.Context, _ *connect.Request[execenvv1.GetReportRequest]) (*connect.Response[execenvv1.Report], error) {
	report, err := h.inner.Ready(ctx)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ReportTo(report)), nil
}

func (h *server) EnsureGrant(ctx context.Context, req *connect.Request[execenvv1.EnsureGrantRequest]) (*connect.Response[execenvv1.EnsureGrantResponse], error) {
	spec, ok := wire.SpecFrom(req.Msg.GetSpec())
	if !ok || execenv.ValidateSpec(spec) != nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	unlock, err := h.lockGrant(ctx, spec.ID, cancel, false)
	if err != nil {
		return nil, wire.Error(err)
	}
	defer unlock()
	env, err := h.inner.Ensure(ctx, spec)
	if err != nil {
		return nil, wire.Error(err)
	}
	h.mu.Lock()
	h.envs[spec.ID] = env
	h.mu.Unlock()
	return connect.NewResponse(&execenvv1.EnsureGrantResponse{Epoch: env.Epoch()}), nil
}

func (h *server) RevokeGrant(ctx context.Context, req *connect.Request[execenvv1.RevokeGrantRequest]) (*connect.Response[execenvv1.RevokeGrantResponse], error) {
	id := execenv.ID(req.Msg.GetGrantId())
	if err := execenv.ValidateGrantID(id); err != nil {
		return nil, wire.Error(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, wire.Error(err)
	}
	unlock, err := h.lockGrant(ctx, id, nil, req.Msg.GetEpoch() == "")
	if err != nil {
		return nil, wire.Error(err)
	}
	defer unlock()
	if epoch := req.Msg.GetEpoch(); epoch != "" {
		env, err := h.env(id, epoch)
		if errors.Is(err, execenv.ErrRevoked) || errors.Is(err, execenv.ErrConflict) {
			return connect.NewResponse(&execenvv1.RevokeGrantResponse{}), nil
		}
		if err != nil {
			return nil, wire.Error(err)
		}
		if err := env.Revoke(ctx); err != nil {
			return nil, wire.Error(err)
		}
	} else if err := h.inner.Revoke(ctx, id); err != nil {
		return nil, wire.Error(err)
	}
	h.forget(id)
	return connect.NewResponse(&execenvv1.RevokeGrantResponse{}), nil
}

// ControlService

func (h *server) Control(ctx context.Context, req *connect.Request[execenvv1.ControlRequest]) (*connect.Response[execenvv1.ControlReceipt], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	receipt, err := env.Control(ctx, execenv.ControlRequest{Fence: fence, State: wire.ControlStateFrom(req.Msg.GetState())})
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ReceiptTo(receipt)), nil
}

func (h *server) ControlStatus(ctx context.Context, req *connect.Request[execenvv1.ControlStatusRequest]) (*connect.Response[execenvv1.ControlReceipt], error) {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return nil, wire.Error(err)
	}
	receipt, err := env.ControlStatus(ctx)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ReceiptTo(receipt)), nil
}

func (h *server) UpdateLimits(ctx context.Context, req *connect.Request[execenvv1.UpdateLimitsRequest]) (*connect.Response[execenvv1.UpdateLimitsResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	if err := env.UpdateLimits(ctx, fence, wire.LimitsFrom(req.Msg.GetLimits())); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.UpdateLimitsResponse{}), nil
}

// JournalService

func (h *server) UploadContent(ctx context.Context, stream *connect.ClientStream[execenvv1.UploadContentRequest]) (*connect.Response[execenvv1.ContentReference], error) {
	if !stream.Receive() {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	header := stream.Msg().GetFence()
	if header == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	env, fence, err := h.fenced(header)
	if err != nil {
		return nil, wire.Error(err)
	}
	body := &wire.ChunkReader{Next: func() ([]byte, bool, error) {
		if !stream.Receive() {
			return nil, false, stream.Err()
		}
		chunk := stream.Msg().GetChunk()
		if chunk == nil {
			return nil, false, execenv.ErrInvalid
		}
		return chunk, true, nil
	}}
	ref, err := env.UploadProjectionContent(ctx, fence, body)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ContentTo(&ref)), nil
}

func (h *server) InitializeProjection(ctx context.Context, req *connect.Request[execenvv1.InitializeProjectionRequest]) (*connect.Response[execenvv1.ProjectionReceipt], error) {
	env, _, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	receipt, err := env.InitializeProjection(ctx, wire.InitializationFrom(req.Msg))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ProjectionReceiptTo(receipt)), nil
}

func (h *server) ApplyProjection(ctx context.Context, req *connect.Request[execenvv1.ApplyProjectionRequest]) (*connect.Response[execenvv1.ProjectionReceipt], error) {
	env, _, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	receipt, err := env.ApplyProjection(ctx, wire.ApplyFrom(req.Msg))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ProjectionReceiptTo(receipt)), nil
}

func (h *server) Observe(ctx context.Context, req *connect.Request[execenvv1.ObserveRequest]) (*connect.Response[execenvv1.ObservationPage], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	request := wire.ObserveRequestFrom(req.Msg)
	request.Fence = fence
	page, err := env.Observe(ctx, request)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ObservationPageTo(page)), nil
}

func (h *server) ReadObservationContent(ctx context.Context, req *connect.Request[execenvv1.ReadObservationContentRequest], stream *connect.ServerStream[execenvv1.ContentChunk]) error {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return wire.Error(err)
	}
	ref := wire.ContentFrom(req.Msg.GetContent())
	if ref == nil {
		return wire.Error(execenv.ErrInvalid)
	}
	body, err := env.OpenObservationContent(ctx, fence, *ref)
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
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return wire.Error(err)
		}
	}
}

func (h *server) ConfirmObservationProjection(ctx context.Context, req *connect.Request[execenvv1.ConfirmObservationProjectionRequest]) (*connect.Response[execenvv1.ConfirmObservationProjectionResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	binding := execenv.ObservationProjection{Fence: fence, HostSequence: req.Msg.GetHostSequence(), Mutation: wire.MutationFrom(req.Msg.GetMutation())}
	if err := env.ConfirmObservationProjection(ctx, binding); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.ConfirmObservationProjectionResponse{}), nil
}

func (h *server) AcknowledgeObservations(ctx context.Context, req *connect.Request[execenvv1.AcknowledgeObservationsRequest]) (*connect.Response[execenvv1.AcknowledgeObservationsResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	if err := env.AcknowledgeObservations(ctx, fence, req.Msg.GetThrough()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.AcknowledgeObservationsResponse{}), nil
}

// ProcessLogService

func (h *server) ObserveProcesses(ctx context.Context, req *connect.Request[execenvv1.ObserveProcessesRequest]) (*connect.Response[execenvv1.ProcessPage], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	page, err := env.ObserveProcesses(ctx, fence, req.Msg.GetAfter(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ProcessPageTo(page)), nil
}

func (h *server) AcknowledgeProcesses(ctx context.Context, req *connect.Request[execenvv1.AcknowledgeProcessesRequest]) (*connect.Response[execenvv1.AcknowledgeProcessesResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	if err := env.AcknowledgeProcesses(ctx, fence, req.Msg.GetThrough()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.AcknowledgeProcessesResponse{}), nil
}

// discardLog keeps the HTTP server's own messages, which can quote request
// lines, out of the host's logs.
var discardLog = log.New(io.Discard, "", 0)
