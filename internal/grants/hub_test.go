package grants

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

type fake struct {
	mu     sync.Mutex
	grants map[execenv.ID]execenv.GrantInfo
}

func (f *fake) snap() ([]execenv.GrantInfo, execenv.HostCapacity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]execenv.GrantInfo, 0, len(f.grants))
	for _, g := range f.grants {
		out = append(out, g)
	}
	return out, execenv.HostCapacity{}
}

func (f *fake) set(g execenv.GrantInfo) {
	f.mu.Lock()
	f.grants[g.ID] = g
	f.mu.Unlock()
}

// A watcher that does not read holds only each grant's latest state: a
// burst of changes arrives as the last of them.
func TestSlowWatcherSeesTheLatestState(t *testing.T) {
	f := &fake{grants: map[execenv.ID]execenv.GrantInfo{"g": {ID: "g", State: execenv.GrantRunning}}}
	h := New(f.snap)
	w, err := h.WatchGrant("g")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if g, err := w.Next(ctx); err != nil || g.State != execenv.GrantRunning {
		t.Fatalf("first = %+v, %v", g, err)
	}
	for rev := uint64(1); rev <= 50; rev++ {
		state := execenv.GrantFrozen
		if rev%2 == 0 {
			state = execenv.GrantRunning
		}
		f.set(execenv.GrantInfo{ID: "g", State: state, FenceRevision: rev})
		h.compare()
	}
	g, err := w.Next(ctx)
	if err != nil || g.FenceRevision != 50 {
		t.Fatalf("after a burst = %+v, %v; want revision 50", g, err)
	}
	quiet, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if g, err := w.Next(quiet); err == nil {
		t.Fatalf("a burst left more than its last state: %+v", g)
	}
}

// A poke is seen without the backstop, and the backstop goroutine runs
// only while someone watches.
func TestPokeAndBackstop(t *testing.T) {
	f := &fake{grants: map[execenv.ID]execenv.GrantInfo{}}
	h := New(f.snap)
	w := h.WatchHost()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if ev, err := w.Next(ctx); err != nil || ev.Capacity == nil {
		t.Fatalf("first = %+v, %v", ev, err)
	}
	f.set(execenv.GrantInfo{ID: "new", State: execenv.GrantRunning})
	h.Poke()
	if ev, err := w.Next(ctx); err != nil || ev.Grant == nil || ev.Grant.ID != "new" {
		t.Fatalf("after a poke = %+v, %v", ev, err)
	}
	// Unpoked, the backstop still finds a change.
	f.set(execenv.GrantInfo{ID: "quiet", State: execenv.GrantRunning})
	if ev, err := w.Next(ctx); err != nil || ev.Grant == nil || ev.Grant.ID != "quiet" {
		t.Fatalf("by the backstop = %+v, %v", ev, err)
	}
	_ = w.Close()
	h.mu.Lock()
	ticking := h.ticking != nil
	h.mu.Unlock()
	if ticking {
		t.Fatal("the backstop runs with nobody watching")
	}
}
