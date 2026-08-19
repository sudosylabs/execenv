package remote

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"io"
	"net"
	"sync"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/mux"
)

// Serve accepts connections on ln and serves inner. Each connection is one
// client. Closing a PTY or Watch stream does not revoke grants on inner.
func Serve(ctx context.Context, ln net.Listener, inner execenv.Host, cfg ServerConfig) error {
	if err := validateServer(cfg); err != nil {
		return err
	}
	if inner == nil {
		return execenv.Error("serve", execenv.ErrInvalid)
	}
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
		go serveConn(ctx, conn, inner, cfg)
	}
}

type serverConn struct {
	sess  *mux.Session
	inner execenv.Host
	cfg   ServerConfig
	mu    sync.Mutex
	authed bool
	envs   map[execenv.ID]execenv.Env
	terms  map[execenv.ID]execenv.Terminal
	obs    map[execenv.ID]execenv.Observation
}

func serveConn(ctx context.Context, conn net.Conn, inner execenv.Host, cfg ServerConfig) {
	sc := &serverConn{
		sess:  newSession(conn),
		inner: inner,
		cfg:   cfg,
		envs:  make(map[execenv.ID]execenv.Env),
		terms: make(map[execenv.ID]execenv.Terminal),
		obs:   make(map[execenv.ID]execenv.Observation),
	}
	defer sc.close()
	for {
		f, err := sc.sess.Recv()
		if err != nil {
			return
		}
		switch f.Kind {
		case kindPty:
			sc.writePty(f)
		case kindRequest:
			sc.handle(ctx, f)
		}
	}
}

func (sc *serverConn) close() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for id, term := range sc.terms {
		_ = term.Close()
		delete(sc.terms, id)
	}
	for id, obs := range sc.obs {
		_ = obs.Close()
		delete(sc.obs, id)
	}
	// Grants stay on the inner host. A dropped client is hangup, not revoke.
	_ = sc.sess.Close()
}

func (sc *serverConn) handle(ctx context.Context, f frame) {
	if f.Method != methodAuth && !sc.authed {
		_ = sc.reply(f, execenv.ErrInvalid, nil)
		return
	}
	var err error
	var extra []byte
	switch f.Method {
	case methodAuth:
		err = sc.auth(f)
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
		err = sc.watch(ctx, f)
	case methodUnwatch:
		err = sc.unwatch(f)
	case methodOpen:
		extra, err = sc.open(ctx, f)
	default:
		err = execenv.ErrInvalid
	}
	_ = sc.reply(f, err, extra)
}

func (sc *serverConn) reply(req frame, err error, extra []byte) error {
	return sc.sess.Send(frame{
		Seq:    req.Seq,
		Kind:   kindResponse,
		Method: req.Method,
		Grant:  req.Grant,
		Status: statusOf(err),
		Extra:  extra,
	})
}

func (sc *serverConn) auth(f frame) error {
	var args authArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	if len(sc.cfg.Token) > 0 && subtle.ConstantTimeCompare(sc.cfg.Token, args.Token) != 1 {
		return execenv.ErrInvalid
	}
	sc.authed = true
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
	if err := decodeExtra(f.Extra, &args); err != nil {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.inner.Ensure(ctx, args.Spec)
	if err != nil {
		return nil, err
	}
	sc.mu.Lock()
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
	if term := sc.terms[id]; term != nil {
		_ = term.Close()
		delete(sc.terms, id)
	}
	if obs := sc.obs[id]; obs != nil {
		_ = obs.Close()
		delete(sc.obs, id)
	}
	delete(sc.envs, id)
	sc.mu.Unlock()
	return sc.inner.Revoke(ctx, id)
}

func (sc *serverConn) attach(ctx context.Context, f frame) error {
	var args attachArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return err
	}
	term, err := env.Attach(ctx, args.Window)
	if err != nil {
		return err
	}
	sc.mu.Lock()
	sc.terms[execenv.ID(f.Grant)] = term
	sc.mu.Unlock()
	go sc.copyPty(execenv.ID(f.Grant), term)
	return nil
}

func (sc *serverConn) copyPty(id execenv.ID, term execenv.Terminal) {
	buf := make([]byte, 32*1024)
	for {
		n, err := term.Read(buf)
		if n > 0 {
			_ = sc.sess.Send(frame{
				Kind:   kindPty,
				Grant:  string(id),
				Extra:  append([]byte(nil), buf[:n]...),
			})
		}
		if err != nil {
			_ = sc.sess.Send(frame{
				Kind:   kindPty,
				Grant:  string(id),
				Status: statusOf(err),
			})
			return
		}
	}
}

func (sc *serverConn) writePty(f frame) {
	sc.mu.Lock()
	term := sc.terms[execenv.ID(f.Grant)]
	sc.mu.Unlock()
	if term == nil {
		return
	}
	_, _ = term.Write(f.Extra)
}

func (sc *serverConn) detach(f frame) error {
	id := execenv.ID(f.Grant)
	sc.mu.Lock()
	term := sc.terms[id]
	delete(sc.terms, id)
	sc.mu.Unlock()
	if term != nil {
		return term.Close()
	}
	return nil
}

func (sc *serverConn) resize(ctx context.Context, f frame) error {
	var args resizeArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	sc.mu.Lock()
	term := sc.terms[execenv.ID(f.Grant)]
	sc.mu.Unlock()
	if term == nil {
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

func (sc *serverConn) watch(ctx context.Context, f frame) error {
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return err
	}
	obs, err := env.Watch(ctx)
	if err != nil {
		return err
	}
	id := execenv.ID(f.Grant)
	sc.mu.Lock()
	sc.obs[id] = obs
	sc.mu.Unlock()
	go sc.copyWatch(id, obs)
	return nil
}

func (sc *serverConn) copyWatch(id execenv.ID, obs execenv.Observation) {
	for {
		ev, err := obs.Next(context.Background())
		if err != nil {
			_ = sc.sess.Send(frame{
				Kind:   kindWatch,
				Grant:  string(id),
				Status: statusOf(err),
			})
			return
		}
		extra, err := encodeExtra(ev)
		if err != nil {
			return
		}
		_ = sc.sess.Send(frame{
			Kind:  kindWatch,
			Grant: string(id),
			Extra: extra,
		})
	}
}

func (sc *serverConn) unwatch(f frame) error {
	id := execenv.ID(f.Grant)
	sc.mu.Lock()
	obs := sc.obs[id]
	delete(sc.obs, id)
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
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	return encodeExtra(data)
}
