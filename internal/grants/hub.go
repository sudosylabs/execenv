// Package grants follows a host's grants for ListGrants, WaitGrant and
// the watch streams. The host pokes the hub whenever a grant may have
// changed; the hub compares snapshots and tells each watcher what changed.
// A watcher holds at most one pending state per grant, so a slow one costs
// no more than the grants themselves.
package grants

import (
	"context"
	"io"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sudosylabs/execenv"
)

// Snapshot is the host's grants and capacity now. Grants that have ended
// are left out; the hub reports them revoked or lost from what it saw last.
type Snapshot func() ([]execenv.GrantInfo, execenv.HostCapacity)

// backstop is how often the hub compares snapshots while anyone watches,
// even when nothing poked it, so a change a host forgot to report is still
// seen. With nobody watching it runs nothing.
const backstop = time.Second

// Hub serves one host's grant calls. It holds no goroutine of its own
// while idle: a poke compares on a short-lived one, and the backstop runs
// only while a watch is open.
type Hub struct {
	snap Snapshot
	// cmp keeps comparisons in order: each takes its snapshot after the
	// one before has been told.
	cmp     sync.Mutex
	pending atomic.Bool
	running atomic.Bool

	mu      sync.Mutex
	last    map[execenv.ID]execenv.GrantInfo
	cap     execenv.HostCapacity
	subs    map[*sub]struct{}
	ticking chan struct{}
}

// New is a hub over snap.
func New(snap Snapshot) *Hub {
	h := &Hub{snap: snap, last: map[execenv.ID]execenv.GrantInfo{}, subs: map[*sub]struct{}{}}
	h.compare()
	return h
}

// Poke says a grant or the capacity may have changed. It never blocks and
// may be called with the host's own locks held.
func (h *Hub) Poke() {
	h.pending.Store(true)
	if h.running.CompareAndSwap(false, true) {
		go h.drain()
	}
}

func (h *Hub) drain() {
	for {
		for h.pending.Swap(false) {
			h.compare()
		}
		h.running.Store(false)
		if !h.pending.Load() || !h.running.CompareAndSwap(false, true) {
			return
		}
	}
}

// tick runs the backstop until stop closes.
func (h *Hub) tick(stop <-chan struct{}) {
	t := time.NewTicker(backstop)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.Poke()
		}
	}
}

// compare takes a snapshot and tells every watcher what differs from the
// last one.
func (h *Hub) compare() {
	h.cmp.Lock()
	defer h.cmp.Unlock()
	now, capacity := h.snap()
	seen := make(map[execenv.ID]execenv.GrantInfo, len(now))
	for _, g := range now {
		seen[g.ID] = g
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var changed []execenv.GrantInfo
	for id, g := range seen {
		if old, ok := h.last[id]; !ok || !same(old, g) {
			changed = append(changed, g)
		}
	}
	for id, old := range h.last {
		if _, ok := seen[id]; !ok {
			// Gone from the host: revoked, unless it was lost and the
			// revoke of a lost grant is what removed it.
			old.State = execenv.GrantRevoked
			changed = append(changed, old)
		}
	}
	h.last = seen
	capChanged := capacity != h.cap
	h.cap = capacity
	for s := range h.subs {
		for _, g := range changed {
			s.grant(g)
		}
		if capChanged {
			s.capacity(capacity)
		}
	}
}

func same(a, b execenv.GrantInfo) bool {
	return a.Epoch == b.Epoch && a.State == b.State && a.FenceRevision == b.FenceRevision && a.Resources == b.Resources && maps.Equal(a.Labels, b.Labels)
}

// List is every grant now.
func (h *Hub) List() []execenv.GrantInfo {
	h.compare()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]execenv.GrantInfo, 0, len(h.last))
	for _, g := range h.last {
		out = append(out, g)
	}
	return out
}

// Wait returns once id has left GrantCreating.
func (h *Hub) Wait(ctx context.Context, id execenv.ID) (execenv.GrantInfo, error) {
	w, err := h.WatchGrant(id)
	if err != nil {
		return execenv.GrantInfo{}, err
	}
	defer w.Close()
	for {
		g, err := w.Next(ctx)
		if err != nil {
			return execenv.GrantInfo{}, err
		}
		if g.State != execenv.GrantCreating {
			return g, nil
		}
	}
}

// WatchGrant follows id, starting with its state now.
func (h *Hub) WatchGrant(id execenv.ID) (execenv.GrantWatch, error) {
	h.compare()
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.last[id]
	if !ok {
		return nil, execenv.ErrNotFound
	}
	s := h.subscribe(&id)
	s.grant(g)
	return grantWatch{s}, nil
}

// WatchHost follows every grant and the capacity, starting with them now.
func (h *Hub) WatchHost() execenv.HostWatch {
	h.compare()
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.subscribe(nil)
	for _, g := range h.last {
		s.grant(g)
	}
	s.capacity(h.cap)
	return hostWatch{s}
}

// subscribe adds a watcher; the caller holds h.mu.
func (h *Hub) subscribe(only *execenv.ID) *sub {
	s := &sub{hub: h, only: only, pending: map[execenv.ID]execenv.GrantInfo{}}
	s.cond = sync.NewCond(&s.mu)
	h.subs[s] = struct{}{}
	if h.ticking == nil {
		h.ticking = make(chan struct{})
		go h.tick(h.ticking)
	}
	return s
}

// drop removes a watcher; the caller holds h.mu.
func (h *Hub) drop(s *sub) {
	delete(h.subs, s)
	if len(h.subs) == 0 && h.ticking != nil {
		close(h.ticking)
		h.ticking = nil
	}
}

func (h *Hub) unsubscribe(s *sub) {
	h.mu.Lock()
	h.drop(s)
	h.mu.Unlock()
	s.end(execenv.ErrClosed)
}

// sub is one watcher's pending changes: the latest state of each grant
// that changed, in the order they first changed, and the capacity.
type sub struct {
	hub  *Hub
	only *execenv.ID

	mu      sync.Mutex
	cond    *sync.Cond
	order   []execenv.ID
	pending map[execenv.ID]execenv.GrantInfo
	cap     *execenv.HostCapacity
	err     error
}

func (s *sub) grant(g execenv.GrantInfo) {
	if s.only != nil && *s.only != g.ID {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[g.ID]; !ok {
		s.order = append(s.order, g.ID)
	}
	s.pending[g.ID] = g
	s.cond.Broadcast()
}

func (s *sub) capacity(c execenv.HostCapacity) {
	if s.only != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cap = &c
	s.cond.Broadcast()
}

func (s *sub) end(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
	s.cond.Broadcast()
}

// next waits for the next pending change, or ctx.
func (s *sub) next(ctx context.Context) (execenv.HostEvent, error) {
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.order) == 0 && s.cap == nil && s.err == nil && ctx.Err() == nil {
		s.cond.Wait()
	}
	if len(s.order) > 0 {
		id := s.order[0]
		s.order = s.order[1:]
		g := s.pending[id]
		delete(s.pending, id)
		return execenv.HostEvent{Grant: &g}, nil
	}
	if s.cap != nil {
		c := *s.cap
		s.cap = nil
		return execenv.HostEvent{Capacity: &c}, nil
	}
	if s.err != nil {
		return execenv.HostEvent{}, s.err
	}
	return execenv.HostEvent{}, ctx.Err()
}

type grantWatch struct{ s *sub }

func (w grantWatch) Next(ctx context.Context) (execenv.GrantInfo, error) {
	w.s.mu.Lock()
	ended := w.s.err == io.EOF
	w.s.mu.Unlock()
	if ended {
		return execenv.GrantInfo{}, io.EOF
	}
	ev, err := w.s.next(ctx)
	if err != nil {
		return execenv.GrantInfo{}, err
	}
	if ev.Grant.State == execenv.GrantRevoked {
		// The last state: the watch ends after it.
		w.s.end(io.EOF)
		w.s.hub.mu.Lock()
		w.s.hub.drop(w.s)
		w.s.hub.mu.Unlock()
	}
	return *ev.Grant, nil
}

func (w grantWatch) Close() error { w.s.hub.unsubscribe(w.s); return nil }

type hostWatch struct{ s *sub }

func (w hostWatch) Next(ctx context.Context) (execenv.HostEvent, error) { return w.s.next(ctx) }
func (w hostWatch) Close() error                                        { w.s.hub.unsubscribe(w.s); return nil }
