package remote

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"net"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/mux"
)

const maxConnectionOperations = 128

// Serve accepts connections on ln and serves inner. Each connection is one
// client. Closing a PTY or losing a connection detaches its terminals: their
// shells keep running on inner for its terminal grace, and grants stay.
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
	if inner == nil {
		return execenv.Error("serve", execenv.ErrInvalid)
	}
	release := cfg.claimed()
	if cfg.Security == SecurityTLS {
		ln = tls.NewListener(ln, cfg.TLS)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	uploads := newProjectionUploads()
	downloads := newSemanticDownloads()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return execenv.Error("serve", err)
		}
		go serveConn(ctx, conn, inner, cfg, release, uploads, downloads)
	}
}

type serverConn struct {
	uploads   *projectionUploads
	downloads *semanticDownloads
	sess      *mux.Session
	conn      net.Conn
	inner     execenv.Host
	cfg       ServerConfig
	release   string
	ctx       context.Context
	cancel    context.CancelFunc

	mu     sync.Mutex
	closed bool
	envs   map[execenv.ID]execenv.Env
	// streams holds the connection's current terminal stream per grant.
	streams  map[execenv.ID]*serverStream
	tails    map[string]chan struct{}
	inflight chan struct{}
}

func serveConn(parent context.Context, conn net.Conn, inner execenv.Host, cfg ServerConfig, release string, uploads *projectionUploads, downloads *semanticDownloads) {
	ctx, cancel := context.WithCancel(parent)
	sc := &serverConn{
		uploads:   uploads,
		downloads: downloads,
		sess:      newSession(conn),
		conn:      conn,
		inner:     inner,
		cfg:       cfg,
		release:   release,
		ctx:       ctx,
		cancel:    cancel,
		envs:      make(map[execenv.ID]execenv.Env),
		streams:   make(map[execenv.ID]*serverStream),
		tails:     make(map[string]chan struct{}),
		inflight:  make(chan struct{}, maxConnectionOperations),
	}
	defer sc.close()

	_ = conn.SetReadDeadline(time.Now().Add(timeoutOrDefault(cfg.AuthTimeout)))
	first, err := sc.sess.Recv()
	if err != nil {
		return
	}
	if first.Kind != kindRequest || first.Method != methodAuth {
		_ = sc.reply(first, execenv.ErrInvalid, nil)
		return
	}
	err = sc.auth(first)
	var authExtra []byte
	if err == nil {
		authExtra, err = encodeExtra(authResult{Protocol: protocolVersion, Capabilities: inner.Capabilities()})
	}
	_ = sc.reply(first, err, authExtra)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	for {
		f, err := sc.sess.Recv()
		if err != nil {
			return
		}
		// PTY input and credit never wait here and never count against
		// the operation limit: each stream's credit bounds what it queues.
		switch f.Kind {
		case kindRequest:
			sc.dispatch(f)
		case kindPty:
			sc.writePty(f)
		case kindCredit:
			sc.creditPty(f)
		}
	}
}

// dispatch admits a bounded number of operations, preserves arrival order
// within one grant, and lets different grants progress independently.
func (sc *serverConn) dispatch(f frame) {
	select {
	case sc.inflight <- struct{}{}:
	default:
		_ = sc.reply(f, execenv.ErrBusy, nil)
		return
	}
	key := f.Grant
	if key == "" {
		key = "$host"
	}
	done := make(chan struct{})
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		<-sc.inflight
		return
	}
	previous := sc.tails[key]
	sc.tails[key] = done
	sc.mu.Unlock()
	go func() {
		defer func() {
			close(done)
			<-sc.inflight
			sc.mu.Lock()
			if sc.tails[key] == done {
				delete(sc.tails, key)
			}
			sc.mu.Unlock()
		}()
		if previous != nil {
			select {
			case <-previous:
			case <-sc.ctx.Done():
				return
			}
		}
		sc.handle(f)
	}()
}

func (sc *serverConn) close() {
	sc.cancel()
	if sc.downloads != nil {
		sc.downloads.close(sc)
	}
	if sc.uploads != nil {
		sc.uploads.close(sc)
	}
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return
	}
	sc.closed = true
	streams := make([]*serverStream, 0, len(sc.streams))
	for _, s := range sc.streams {
		streams = append(streams, s)
	}
	sc.streams = make(map[execenv.ID]*serverStream)
	sc.mu.Unlock()
	// A lost connection is a detach, never a hangup.
	for _, s := range streams {
		s.shut(nil)
		_ = s.term.Close()
	}
	_ = sc.sess.Close()
}

func (sc *serverConn) handle(f frame) {
	if epochMethod(f.Method) {
		if err := sc.unwrapEpoch(&f); err != nil {
			_ = sc.reply(f, err, nil)
			return
		}
	}
	ctx, cancel := sc.operationContext(f)
	defer cancel()
	var err error
	var extra []byte
	switch f.Method {
	case methodObserve, methodObservationAcknowledge, methodObservationOpen, methodObservationRead, methodObservationClose, methodObservationConfirm:
		extra, err = sc.semantic(ctx, f)
	case methodProjectionInitialize:
		extra, err = sc.initializeProjection(ctx, f)
	case methodProjectionApply:
		extra, err = sc.applyProjection(ctx, f)
	case methodProjectionUploadBegin, methodProjectionUploadChunk, methodProjectionUploadFinish, methodProjectionUploadAbort:
		extra, err = sc.projectionUpload(ctx, f)
	case methodControl:
		extra, err = sc.control(ctx, f)
	case methodControlStatus:
		extra, err = sc.controlStatus(ctx, f)
	case methodAuth:
		err = execenv.ErrInvalid
	case methodReady:
		extra, err = sc.ready(ctx)
	case methodEnsure:
		extra, err = sc.ensure(ctx, f)
	case methodRevokeEpoch:
		err = sc.revokeEpoch(ctx, f)
	case methodRevoke:
		err = sc.revoke(ctx, f)
	case methodAttach:
		extra, err = sc.attach(ctx, f)
	case methodDetach:
		err = sc.detach(ctx, f)
	case methodHangup:
		err = sc.hangup(ctx, f)
	case methodResize:
		err = sc.resize(ctx, f)
	default:
		err = execenv.ErrInvalid
	}
	_ = sc.reply(f, err, extra)
}

func (sc *serverConn) operationContext(f frame) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(operationTimeoutOrDefault(sc.cfg.OperationTimeout))
	if f.Deadline > 0 {
		requested := time.Unix(0, f.Deadline)
		if requested.Before(deadline) {
			deadline = requested
		}
	}
	return context.WithDeadline(sc.ctx, deadline)
}

func (sc *serverConn) reply(req frame, err error, extra []byte) error {
	return sc.sess.Send(frame{Seq: req.Seq, Kind: kindResponse, Method: req.Method, Grant: req.Grant, Status: statusOf(err), Extra: extra})
}

func (sc *serverConn) auth(f frame) error {
	var args authArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	tokenOK := len(sc.cfg.Token) > 0 && subtle.ConstantTimeCompare(sc.cfg.Token, args.Token) == 1
	certificateOK := false
	if tlsConn, ok := sc.conn.(*tls.Conn); ok {
		state := tlsConn.ConnectionState()
		certificateOK = len(state.PeerCertificates) > 0 && len(state.VerifiedChains) > 0
	}
	if !tokenOK && !certificateOK {
		return execenv.ErrInvalid
	}
	if args.Release != sc.release || args.Protocol != protocolVersion {
		return execenv.ErrUnavailable
	}
	return nil
}

func (sc *serverConn) ready(ctx context.Context) ([]byte, error) {
	report, err := sc.inner.Ready(ctx)
	if err != nil {
		return nil, err
	}
	return encodeExtra(report)
}

func (sc *serverConn) ensure(ctx context.Context, f frame) ([]byte, error) {
	var args ensureArgs
	if err := decodeExtra(f.Extra, &args); err != nil || string(args.Spec.ID) != f.Grant {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.inner.Ensure(ctx, args.Spec)
	if err != nil {
		return nil, err
	}
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return nil, execenv.ErrConnection
	}
	sc.envs[args.Spec.ID] = env
	sc.mu.Unlock()
	return encodeExtra(ensureResult{Epoch: env.Epoch()})
}

func (sc *serverConn) env(id execenv.ID) (execenv.Env, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	env, ok := sc.envs[id]
	if !ok {
		return nil, execenv.ErrRevoked
	}
	return env, nil
}

func (sc *serverConn) revoke(ctx context.Context, f frame) error {
	id := execenv.ID(f.Grant)
	sc.mu.Lock()
	s := sc.streams[id]
	delete(sc.streams, id)
	delete(sc.envs, id)
	sc.mu.Unlock()
	if s != nil {
		s.shut(nil)
		_ = s.term.Close()
	}
	return sc.inner.Revoke(ctx, id)
}

func (sc *serverConn) attach(ctx context.Context, f frame) ([]byte, error) {
	var args attachArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 || args.Credit == 0 || args.Credit > maxPtyWindow {
		return nil, execenv.ErrInvalid
	}
	id := execenv.ID(f.Grant)
	env, err := sc.env(id)
	if err != nil {
		return nil, err
	}
	term, err := env.Attach(ctx, execenv.AttachRequest{Window: args.Window, Resume: args.Resume, Takeover: args.Takeover})
	if err != nil {
		return nil, err
	}
	extra, err := encodeExtra(attachResult{Terminal: term.ID(), Start: term.Start(), Credit: ptyWindow})
	if err != nil {
		_ = term.Close()
		return nil, err
	}
	s := newServerStream(sc, id, args.Stream, term, args.Credit)
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		_ = term.Close()
		return nil, execenv.ErrConnection
	}
	// A takeover from this same connection replaces the stream; the inner
	// host supersedes the previous Terminal. Its pump may be waiting for
	// credit the client will never send, so shut it here.
	previous := sc.streams[id]
	sc.streams[id] = s
	sc.mu.Unlock()
	if previous != nil {
		previous.shut(execenv.ErrSuperseded)
	}
	go s.pumpOutput()
	return extra, nil
}

// detach ends the stream after input it already received reaches the
// terminal, so a client's last keystrokes are not lost to its own Close.
func (sc *serverConn) detach(ctx context.Context, f frame) error {
	var args streamArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	id := execenv.ID(f.Grant)
	sc.mu.Lock()
	s := sc.streams[id]
	if s == nil || s.stream != args.Stream {
		sc.mu.Unlock()
		return nil
	}
	delete(sc.streams, id)
	sc.mu.Unlock()
	s.waitInputDrained(ctx)
	s.shut(nil)
	return s.term.Close()
}

// hangup ends the shell behind the connection's current stream for the grant.
func (sc *serverConn) hangup(ctx context.Context, f frame) error {
	var args streamArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	s := sc.stream(frame{Grant: f.Grant, Stream: args.Stream})
	if s == nil {
		return execenv.ErrClosed
	}
	return s.term.Hangup(ctx)
}

func (sc *serverConn) resize(ctx context.Context, f frame) error {
	var args resizeArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	s := sc.stream(frame{Grant: f.Grant, Stream: args.Stream})
	if s == nil {
		return execenv.ErrClosed
	}
	return s.term.Resize(ctx, args.Window)
}
