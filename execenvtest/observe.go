package execenvtest

import (
	"errors"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// RunObservations exercises the observation stream against guest-originated
// changes: waiting for one. It needs a real or simulated guest, so it is
// separate from Run.
func RunObservations(t *testing.T, factory ObservedFactory) {
	t.Helper()

	t.Run("a wait with nothing new returns an empty page once it passes", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		began := time.Now()
		page, err := w.o.Workspace.Observe(t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, Limit: 16, Wait: 150 * time.Millisecond})
		if err != nil || len(page.Observations) != 0 {
			t.Fatalf("Observe(wait) = %+v, %v; want an empty page", page, err)
		}
		if waited := time.Since(began); waited < 150*time.Millisecond {
			t.Fatalf("Observe(wait) returned after %v, before its wait", waited)
		}
	})

	t.Run("a wait ends as soon as the guest changes something", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		done := make(chan error, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			done <- w.o.Guest.Write(t.Context(), "late.txt", []byte("x"))
		}()
		began := time.Now()
		page, err := w.o.Workspace.Observe(t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, Limit: 16, Wait: execenv.MaxObserveWait})
		if err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(page.Observations) != 1 || page.Observations[0].Path != "late.txt" {
			t.Fatalf("Observe(wait) = %+v, want the late write", page.Observations)
		}
		if waited := time.Since(began); waited > 10*time.Second {
			t.Fatalf("Observe(wait) took %v after the write", waited)
		}
	})

	t.Run("a wait returns at once when there is already something after its position", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		w.write("early.txt", "x")
		began := time.Now()
		page, err := w.o.Workspace.Observe(t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, Limit: 16, Wait: execenv.MaxObserveWait})
		if err != nil || len(page.Observations) != 1 {
			t.Fatalf("Observe(wait) = %+v, %v", page, err)
		}
		if waited := time.Since(began); waited > 10*time.Second {
			t.Fatalf("Observe(wait) waited %v with an observation pending", waited)
		}
	})

	t.Run("a wait outside its bounds is refused", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		for _, wait := range []time.Duration{-time.Millisecond, execenv.MaxObserveWait + time.Millisecond} {
			if _, err := w.o.Workspace.Observe(t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, Limit: 16, Wait: wait}); !errors.Is(err, execenv.ErrInvalid) {
				t.Fatalf("Observe(wait %v) error = %v, want ErrInvalid", wait, err)
			}
		}
	})
}
