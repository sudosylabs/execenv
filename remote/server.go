package remote

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/mux"
)

const maxConnectionOperations = 128

// Serve accepts connections on ln and serves inner. Each connection is one
// client. Closing a PTY or Watch stream does not revoke grants on inner.
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
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return execenv.Error("serve", err)
		}
		go serveConn(ctx, conn, inner, cfg, release)
	}
}

type serverConn struct {
	sess    *mux.Session
	conn    net.Conn
	inner   execenv.Host
	cfg     ServerConfig
	release string
	ctx     context.Context
	cancel  context.CancelFunc

	mu          sync.Mutex
	closed      bool
	envs        map[execenv.ID]execenv.Env
	terms       map[execenv.ID]execenv.Terminal
	termStreams map[execenv.ID]uint64
	obs         map[execenv.ID]execenv.Observation
	obsStreams  map[execenv.ID]uint64
	tails       map[string]chan struct{}
	inflight    chan struct{}
}

func serveConn(parent context.Context, conn net.Conn, inner execenv.Host, cfg ServerConfig, release string) {
	ctx, cancel := context.WithCancel(parent)
	sc := &serverConn{
		sess:        newSession(conn),
		conn:        conn,
		inner:       inner,
		cfg:         cfg,
		release:     release,
		ctx:         ctx,
		cancel:      cancel,
		envs:        make(map[execenv.ID]execenv.Env),
		terms:       make(map[execenv.ID]execenv.Terminal),
		termStreams: make(map[execenv.ID]uint64),
		obs:         make(map[execenv.ID]execenv.Observation),
		obsStreams:  make(map[execenv.ID]uint64),
		tails:       make(map[string]chan struct{}),
		inflight:    make(chan struct{}, maxConnectionOperations),
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
	_ = sc.reply(first, err, nil)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	for {
		f, err := sc.sess.Recv()
		if err != nil {
			return
		}
		sc.dispatch(f)
	}
}

// dispatch admits a bounded number of operations, preserves arrival order
// within one grant, and lets different grants progress independently.
func (sc *serverConn) dispatch(f frame) {
	if f.Kind != kindRequest && f.Kind != kindPty {
		return
	}
	select {
	case sc.inflight <- struct{}{}:
	default:
		if f.Kind == kindRequest {
			_ = sc.reply(f, execenv.ErrBusy, nil)
		} else {
			// PTY input has no response frame. Close instead of silently
			// dropping octets when the bounded dispatcher is saturated.
			_ = sc.sess.Close()
		}
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
		if f.Kind == kindPty {
			sc.writePty(f)
			return
		}
		sc.handle(f)
	}()
}

func (sc *serverConn) close() {
	sc.cancel()
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return
	}
	sc.closed = true
	terms := make([]execenv.Terminal, 0, len(sc.terms))
	for _, term := range sc.terms {
		terms = append(terms, term)
	}
	observations := make([]execenv.Observation, 0, len(sc.obs))
	for _, obs := range sc.obs {
		observations = append(observations, obs)
	}
	sc.terms = make(map[execenv.ID]execenv.Terminal)
	sc.termStreams = make(map[execenv.ID]uint64)
	sc.obs = make(map[execenv.ID]execenv.Observation)
	sc.obsStreams = make(map[execenv.ID]uint64)
	sc.mu.Unlock()
	for _, term := range terms {
		_ = term.Close()
	}
	for _, obs := range observations {
		_ = obs.Close()
	}
	_ = sc.sess.Close()
}

func (sc *serverConn) handle(f frame) {
	ctx, cancel := sc.operationContext(f)
	defer cancel()
	var err error
	var extra []byte
	switch f.Method {
	case methodAuth:
		err = execenv.ErrInvalid
	case methodReady:
		extra, err = sc.ready(ctx)
	case methodEnsure:
		extra, err = sc.ensure(ctx, f)
	case methodRevoke:
		err = sc.revoke(ctx, f)
	case methodAttach:
		err = sc.attach(ctx, f)
	case methodDetach:
		err = sc.detach(f)
	case methodResize:
		err = sc.resize(ctx, f)
	case methodFreeze:
		err = sc.freeze(ctx, f)
	case methodThaw:
		err = sc.thaw(ctx, f)
	case methodReplace:
		err = sc.replace(ctx, f)
	case methodApply:
		err = sc.apply(ctx, f)
	case methodWatch:
		extra, err = sc.watch(ctx, f)
	case methodUnwatch:
		err = sc.unwatch(f)
	case methodOpen:
		extra, err = sc.open(ctx, f)
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
	if args.Release != sc.release {
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
	return nil, nil
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
	term := sc.terms[id]
	obs := sc.obs[id]
	delete(sc.terms, id)
	delete(sc.termStreams, id)
	delete(sc.obs, id)
	delete(sc.obsStreams, id)
	delete(sc.envs, id)
	sc.mu.Unlock()
	if term != nil {
		_ = term.Close()
	}
	if obs != nil {
		_ = obs.Close()
	}
	return sc.inner.Revoke(ctx, id)
}

func (sc *serverConn) attach(ctx context.Context, f frame) error {
	var args attachArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	id := execenv.ID(f.Grant)
	env, err := sc.env(id)
	if err != nil {
		return err
	}
	term, err := env.Attach(ctx, args.Window)
	if err != nil {
		return err
	}
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		_ = term.Close()
		return execenv.ErrConnection
	}
	sc.terms[id] = term
	sc.termStreams[id] = args.Stream
	sc.mu.Unlock()
	go sc.copyPty(id, args.Stream, term)
	return nil
}

func (sc *serverConn) copyPty(id execenv.ID, stream uint64, term execenv.Terminal) {
	defer func() {
		sc.mu.Lock()
		if sc.terms[id] == term {
			delete(sc.terms, id)
			delete(sc.termStreams, id)
		}
		sc.mu.Unlock()
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := term.Read(buf)
		if n > 0 {
			if sendErr := sc.sess.Send(frame{Kind: kindPty, Grant: string(id), Stream: stream, Extra: append([]byte(nil), buf[:n]...)}); sendErr != nil {
				return
			}
		}
		if err != nil {
			_ = sc.sess.Send(frame{Kind: kindPty, Grant: string(id), Stream: stream, Status: statusOf(err)})
			return
		}
	}
}

func (sc *serverConn) writePty(f frame) {
	sc.mu.Lock()
	term := sc.terms[execenv.ID(f.Grant)]
	stream := sc.termStreams[execenv.ID(f.Grant)]
	sc.mu.Unlock()
	if term != nil && stream == f.Stream {
		_, _ = term.Write(f.Extra)
	}
}

func (sc *serverConn) detach(f frame) error {
	var args streamArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	id := execenv.ID(f.Grant)
	sc.mu.Lock()
	term := sc.terms[id]
	if sc.termStreams[id] == args.Stream {
		delete(sc.terms, id)
		delete(sc.termStreams, id)
	} else {
		term = nil
	}
	sc.mu.Unlock()
	if term != nil {
		return term.Close()
	}
	return nil
}

func (sc *serverConn) resize(ctx context.Context, f frame) error {
	var args resizeArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	sc.mu.Lock()
	term := sc.terms[execenv.ID(f.Grant)]
	stream := sc.termStreams[execenv.ID(f.Grant)]
	sc.mu.Unlock()
	if term == nil || stream != args.Stream {
		return execenv.ErrClosed
	}
	return term.Resize(ctx, args.Window)
}

func (sc *serverConn) freeze(ctx context.Context, f frame) error {
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return err
	}
	return env.Freeze(ctx)
}

func (sc *serverConn) thaw(ctx context.Context, f frame) error {
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return err
	}
	return env.Thaw(ctx)
}

func (sc *serverConn) replace(ctx context.Context, f frame) error {
	var args replaceArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return err
	}
	return env.ReplaceTree(ctx, args.Tree)
}

func (sc *serverConn) apply(ctx context.Context, f frame) error {
	var args applyArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return err
	}
	return env.Apply(ctx, args.Batch)
}

func (sc *serverConn) watch(ctx context.Context, f frame) ([]byte, error) {
	var args watchArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return nil, execenv.ErrInvalid
	}
	id := execenv.ID(f.Grant)
	env, err := sc.env(id)
	if err != nil {
		return nil, err
	}
	obs, err := env.Watch(ctx, args.After)
	if err != nil {
		return nil, err
	}
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		_ = obs.Close()
		return nil, execenv.ErrConnection
	}
	sc.obs[id] = obs
	sc.obsStreams[id] = args.Stream
	sc.mu.Unlock()
	go sc.copyWatch(id, args.Stream, obs)
	return encodeExtra(watchResult{Cursor: obs.Cursor()})
}

func (sc *serverConn) copyWatch(id execenv.ID, stream uint64, obs execenv.Observation) {
	defer func() {
		_ = obs.Close()
		sc.mu.Lock()
		if sc.obs[id] == obs {
			delete(sc.obs, id)
			delete(sc.obsStreams, id)
		}
		sc.mu.Unlock()
	}()
	for {
		ev, err := obs.Next(sc.ctx)
		if err != nil {
			_ = sc.sess.Send(frame{Kind: kindWatch, Grant: string(id), Stream: stream, Status: statusOf(err)})
			return
		}
		extra, err := encodeExtra(ev)
		if err != nil {
			return
		}
		if err := sc.sess.Send(frame{Kind: kindWatch, Grant: string(id), Stream: stream, Extra: extra}); err != nil {
			return
		}
	}
}

func (sc *serverConn) unwatch(f frame) error {
	var args streamArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	id := execenv.ID(f.Grant)
	sc.mu.Lock()
	obs := sc.obs[id]
	if sc.obsStreams[id] == args.Stream {
		delete(sc.obs, id)
		delete(sc.obsStreams, id)
	} else {
		obs = nil
	}
	sc.mu.Unlock()
	if obs != nil {
		return obs.Close()
	}
	return nil
}

func (sc *serverConn) open(ctx context.Context, f frame) ([]byte, error) {
	var args openArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return nil, err
	}
	body, err := env.Open(ctx, args.Path)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, execenv.MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > execenv.MaxFileBytes {
		return nil, execenv.ErrTooLarge
	}
	return encodeExtra(data)
}
