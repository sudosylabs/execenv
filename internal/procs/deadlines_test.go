package procs

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sudosylabs/execenv"
)

type deadlineProc struct {
	*pipeProc
	signals chan execenv.Signal
	frozen  atomic.Bool
}

func (p *deadlineProc) Signal(signal execenv.Signal) error {
	p.signals <- signal
	if p.frozen.Load() {
		return execenv.Error("signal", execenv.ErrFrozen)
	}
	if signal == execenv.SignalKill {
		p.exit()
	}
	return nil
}

func newDeadlineProc(t *testing.T) (*Manager, *deadlineProc) {
	t.Helper()
	p := &deadlineProc{pipeProc: newPipeProc(), signals: make(chan execenv.Signal, 8)}
	t.Cleanup(p.exit)
	return New(func(context.Context, execenv.ProcessSpec) (Backend, error) { return p, nil }), p
}

func expectDeadlineSignal(t *testing.T, p *deadlineProc, want execenv.Signal) {
	t.Helper()
	select {
	case got := <-p.signals:
		if got != want {
			t.Fatalf("signal = %q, want %q", got, want)
		}
	default:
		t.Fatalf("missing signal %q", want)
	}
}

func expectNoDeadlineSignal(t *testing.T, p *deadlineProc) {
	t.Helper()
	select {
	case got := <-p.signals:
		t.Fatalf("unexpected signal %q", got)
	default:
	}
}

func TestDeadlineRetriesFrozenSignals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, Timeout: time.Second}); err != nil {
			t.Fatal(err)
		}
		p.frozen.Store(true)
		time.Sleep(time.Second)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalTerminate)
		time.Sleep(killDelay)
		synctest.Wait()
		expectNoDeadlineSignal(t, p)

		p.frozen.Store(false)
		m.Thaw()
		expectDeadlineSignal(t, p, execenv.SignalTerminate)
		p.frozen.Store(true)
		time.Sleep(killDelay)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalKill)
		p.frozen.Store(false)
		m.Thaw()
		expectDeadlineSignal(t, p, execenv.SignalKill)
	})
}

func TestDeadlineSkipsExitedProcessOnThaw(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, Timeout: time.Second}); err != nil {
			t.Fatal(err)
		}
		m.Freeze()
		time.Sleep(2 * time.Second)
		p.exit()
		synctest.Wait()
		m.Thaw()
		expectNoDeadlineSignal(t, p)
	})
}

func TestAdoptCatchesUpAnOverdueTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		a := Adopted{
			Spec: execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, Timeout: time.Second},
			Info: execenv.ProcessInfo{Running: true, Started: time.Now().Add(-time.Hour)},
		}
		if err := m.Adopt(a, p); err != nil {
			t.Fatal(err)
		}
		expectDeadlineSignal(t, p, execenv.SignalTerminate)
		time.Sleep(killDelay)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalKill)
	})
}

func TestAdoptPreservesExpiredDetachGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old, oldProc := newDeadlineProc(t)
		spec := execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, DetachGrace: time.Second}
		info, err := old.Start(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		old.Freeze()
		time.Sleep(2 * time.Second)
		saved := old.SuspendDeadlines()["p"]
		oldProc.exit()

		m, p := newDeadlineProc(t)
		m.Freeze()
		if err := m.Adopt(Adopted{Spec: spec, Info: info, Deadlines: &saved}, p); err != nil {
			t.Fatal(err)
		}
		a, err := m.Attach("p", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		expectNoDeadlineSignal(t, p)
		m.Thaw()
		expectDeadlineSignal(t, p, execenv.SignalHangup)
		time.Sleep(killDelay)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalKill)
	})
}
