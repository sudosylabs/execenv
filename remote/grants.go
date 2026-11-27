package remote

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/wire"
)

// FeatureGrants is a host that lists and follows its grants.
const FeatureGrants = "grants"

// Host side.

func (h *server) ListGrants(ctx context.Context, _ *connect.Request[execenvv1.ListGrantsRequest]) (*connect.Response[execenvv1.ListGrantsResponse], error) {
	grants, err := h.inner.ListGrants(ctx)
	if err != nil {
		return nil, wire.Error(err)
	}
	out := &execenvv1.ListGrantsResponse{}
	for _, g := range grants {
		out.Grants = append(out.Grants, wire.GrantInfoTo(g))
	}
	return connect.NewResponse(out), nil
}

func (h *server) WaitGrant(ctx context.Context, req *connect.Request[execenvv1.WaitGrantRequest]) (*connect.Response[execenvv1.GrantInfo], error) {
	g, err := h.inner.WaitGrant(ctx, execenv.ID(req.Msg.GetGrantId()))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.GrantInfoTo(g)), nil
}

func (h *server) WatchGrant(ctx context.Context, req *connect.Request[execenvv1.WatchGrantRequest], stream *connect.ServerStream[execenvv1.GrantInfo]) error {
	w, err := h.inner.WatchGrant(ctx, execenv.ID(req.Msg.GetGrantId()))
	if err != nil {
		return wire.Error(err)
	}
	defer w.Close()
	for {
		g, err := w.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return wire.Error(err)
		}
		if err := stream.Send(wire.GrantInfoTo(g)); err != nil {
			return err
		}
	}
}

func (h *server) WatchHost(ctx context.Context, _ *connect.Request[execenvv1.WatchHostRequest], stream *connect.ServerStream[execenvv1.HostEvent]) error {
	w, err := h.inner.WatchHost(ctx)
	if err != nil {
		return wire.Error(err)
	}
	defer w.Close()
	for {
		ev, err := w.Next(ctx)
		if err != nil {
			return wire.Error(err)
		}
		if err := stream.Send(wire.HostEventTo(ev)); err != nil {
			return err
		}
	}
}

// Client side.

func (c *Client) ListGrants(ctx context.Context) ([]execenv.GrantInfo, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.host.ListGrants(ctx, connect.NewRequest(&execenvv1.ListGrantsRequest{}))
	if err != nil {
		return nil, wire.HostError("list_grants", err)
	}
	out := make([]execenv.GrantInfo, 0, len(resp.Msg.GetGrants()))
	for _, g := range resp.Msg.GetGrants() {
		out = append(out, wire.GrantInfoFrom(g))
	}
	return out, nil
}

// WaitGrant waits as long as ctx allows: it has no call timeout.
func (c *Client) WaitGrant(ctx context.Context, id execenv.ID) (execenv.GrantInfo, error) {
	resp, err := c.host.WaitGrant(ctx, connect.NewRequest(&execenvv1.WaitGrantRequest{GrantId: string(id)}))
	if err != nil {
		return execenv.GrantInfo{}, wire.HostError("wait_grant", err)
	}
	return wire.GrantInfoFrom(resp.Msg), nil
}

func (c *Client) WatchGrant(ctx context.Context, id execenv.ID) (execenv.GrantWatch, error) {
	sctx, cancel := context.WithCancel(c.ctx)
	stream, err := c.host.WatchGrant(sctx, connect.NewRequest(&execenvv1.WatchGrantRequest{GrantId: string(id)}))
	if err != nil {
		cancel()
		return nil, wire.HostError("watch_grant", err)
	}
	r := follow(sctx, stream.Receive, stream.Msg, stream.Err, cancel)
	// The host sends the grant's state first, or refuses an unknown one.
	first, err := r.next(ctx)
	if err != nil {
		r.close()
		return nil, wire.HostError("watch_grant", err)
	}
	return &grantWatch{r: r, first: first}, nil
}

func (c *Client) WatchHost(context.Context) (execenv.HostWatch, error) {
	sctx, cancel := context.WithCancel(c.ctx)
	stream, err := c.host.WatchHost(sctx, connect.NewRequest(&execenvv1.WatchHostRequest{}))
	if err != nil {
		cancel()
		return nil, wire.HostError("watch_host", err)
	}
	return &hostWatch{r: follow(sctx, stream.Receive, stream.Msg, stream.Err, cancel)}, nil
}

// follower moves a server stream's messages onto a channel, so Next can
// wait on its context as well.
type follower[T any] struct {
	msgs   chan *T
	err    error
	cancel context.CancelFunc
}

func follow[T any](ctx context.Context, receive func() bool, msg func() *T, end func() error, cancel context.CancelFunc) *follower[T] {
	f := &follower[T]{msgs: make(chan *T), cancel: cancel}
	go func() {
		defer close(f.msgs)
		for receive() {
			select {
			case f.msgs <- msg():
			case <-ctx.Done():
				f.err = ctx.Err()
				return
			}
		}
		f.err = end()
	}()
	return f
}

func (f *follower[T]) next(ctx context.Context) (*T, error) {
	select {
	case m, ok := <-f.msgs:
		if !ok {
			if f.err == nil {
				return nil, io.EOF
			}
			return nil, f.err
		}
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *follower[T]) close() {
	f.cancel()
	for range f.msgs {
	}
}

type grantWatch struct {
	r     *follower[execenvv1.GrantInfo]
	first *execenvv1.GrantInfo
}

func (w *grantWatch) Next(ctx context.Context) (execenv.GrantInfo, error) {
	if m := w.first; m != nil {
		w.first = nil
		return wire.GrantInfoFrom(m), nil
	}
	m, err := w.r.next(ctx)
	if errors.Is(err, io.EOF) {
		return execenv.GrantInfo{}, io.EOF
	}
	if err != nil {
		if ctx.Err() != nil {
			return execenv.GrantInfo{}, err
		}
		return execenv.GrantInfo{}, wire.HostError("watch_grant", err)
	}
	return wire.GrantInfoFrom(m), nil
}

func (w *grantWatch) Close() error { w.r.close(); return nil }

type hostWatch struct {
	r *follower[execenvv1.HostEvent]
}

func (w *hostWatch) Next(ctx context.Context) (execenv.HostEvent, error) {
	m, err := w.r.next(ctx)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, io.EOF) {
			return execenv.HostEvent{}, err
		}
		return execenv.HostEvent{}, wire.HostError("watch_host", err)
	}
	return wire.HostEventFrom(m), nil
}

func (w *hostWatch) Close() error { w.r.close(); return nil }
