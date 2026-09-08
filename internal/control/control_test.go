package control

import (
	"context"
	"errors"
	"github.com/sudosylabs/execenv"
	"testing"
)

func TestFailedIntentStillFencesOlderRequests(t *testing.T) {
	s := New("grant")
	req := execenv.ControlRequest{Fence: execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: s.Epoch(), FenceRevision: 10}, State: execenv.ControlFrozen}
	failed := errors.New("pause failed")
	receipt, err := s.Execute(t.Context(), req, func() error { return failed })
	if !errors.Is(err, failed) || receipt.Confirmed {
		t.Fatalf("failed effect acknowledged: %+v %v", receipt, err)
	}
	stale := req
	stale.Fence.FenceRevision--
	stale.State = execenv.ControlRunning
	if _, err := s.Execute(t.Context(), stale, func() error { t.Fatal("stale effect executed"); return nil }); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal(err)
	}
	if receipt, err = s.Execute(t.Context(), req, func() error { return nil }); err != nil || !receipt.Confirmed {
		t.Fatalf("retry: %+v %v", receipt, err)
	}
	if _, err := s.Execute(t.Context(), req, func() error { t.Fatal("exact retry repeated effect"); return nil }); err != nil {
		t.Fatal(err)
	}
	changed := req
	changed.State = execenv.ControlRunning
	if _, err := s.Execute(t.Context(), changed, func() error { t.Fatal("changed request executed"); return nil }); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal(err)
	}
}

func TestRevokeIsTerminalEvenWhenDestructionFails(t *testing.T) {
	s := New("grant")
	req := execenv.ControlRequest{Fence: execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: s.Epoch(), FenceRevision: 1}, State: execenv.ControlRevoked}
	_, _ = s.Execute(t.Context(), req, func() error { return execenv.ErrUnavailable })
	reopen := req
	reopen.Fence.FenceRevision++
	reopen.State = execenv.ControlRunning
	if _, err := s.Execute(t.Context(), reopen, func() error { t.Fatal("revoked epoch reopened"); return nil }); !errors.Is(err, execenv.ErrRevoked) {
		t.Fatal(err)
	}
	receipt, err := s.Execute(t.Context(), req, func() error { return nil })
	if err != nil || !receipt.Confirmed {
		t.Fatalf("revoke retry: %+v %v", receipt, err)
	}
}

func TestCancelledWaitDoesNotExecuteOrChangeFence(t *testing.T) {
	s := New("grant")
	req := execenv.ControlRequest{Fence: execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: s.Epoch(), FenceRevision: 1}, State: execenv.ControlFrozen}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.Execute(t.Context(), req, func() error { close(started); <-release; return nil })
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	newer := req
	newer.Fence.FenceRevision = 2
	if _, err := s.Execute(ctx, newer, func() error { t.Error("cancelled effect executed"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	<-done
	receipt, err := s.Status(t.Context())
	if err != nil || receipt.Fence.FenceRevision != 1 {
		t.Fatalf("status %+v %v", receipt, err)
	}
}
