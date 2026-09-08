package workload

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

type testFreezer struct {
	mu          sync.Mutex
	state       string
	writes      []string
	acknowledge bool
	failThaw    bool
}

func (f *testFreezer) write(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, value)
	if value == "0" && f.failThaw {
		return errors.New("thaw failed")
	}
	if f.acknowledge {
		f.state = value
	}
	return nil
}
func (f *testFreezer) read(string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte("populated 1\nfrozen " + f.state + "\n"), nil
}
func TestGateWaitsForKernelAcknowledgement(t *testing.T) {
	fs := &testFreezer{state: "0"}
	g := gate{fs: fs}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	ran := false
	if err := g.stopped(ctx, func() error { ran = true; return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unacknowledged freeze: %v", err)
	}
	if ran {
		t.Fatal("effect ran while workload might still write")
	}
	if len(fs.writes) != 1 || fs.writes[0] != "1" {
		t.Fatal("failure resumed workload")
	}
	if err := g.stopped(t.Context(), func() error { return nil }); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("uncertain occupancy reused: %v", err)
	}
}
func TestGateContainsFailedCommitAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		commitFailure, thawFailure bool
	}{
		{name: "successful commit"}, {name: "failed commit", commitFailure: true}, {name: "failed recovery", thawFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &testFreezer{state: "0", acknowledge: true, failThaw: tc.thawFailure}
			g := gate{fs: fs}
			failure := errors.New("disk failure")
			err := g.stopped(t.Context(), func() error {
				if fs.state != "1" {
					t.Fatal("effect ran before acknowledgement")
				}
				if tc.commitFailure {
					return failure
				}
				return nil
			})
			if tc.commitFailure {
				if !errors.Is(err, failure) || fs.state != "1" {
					t.Fatalf("failed commit resumed: %v %s", err, fs.state)
				}
			} else if tc.thawFailure {
				if err == nil || fs.writes[len(fs.writes)-1] != "1" {
					t.Fatal("failed thaw not contained")
				}
			} else if err != nil || fs.state != "0" {
				t.Fatalf("success did not resume: %v", err)
			}
			if g.failed != (tc.commitFailure || tc.thawFailure) {
				t.Fatal("incorrect occupancy usability")
			}
		})
	}
}
func TestFreezerEventsRejectMalformedAcknowledgements(t *testing.T) {
	for _, raw := range []string{"", "frozen 2\n", "frozen 1 extra\n", "frozen 0\nfrozen 1\n"} {
		if _, ok := eventValue([]byte(raw), "frozen"); ok {
			t.Fatalf("accepted %q", raw)
		}
	}
	value, ok := eventValue([]byte("populated 1\nfrozen 1\n"), "frozen")
	if !ok || value != 1 {
		t.Fatal("valid kernel state refused")
	}
}
