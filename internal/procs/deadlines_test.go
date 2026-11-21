package procs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sudosylabs/execenv"
)

type deadlineProc struct {
	*pipeProc
	signals  chan execenv.Signal
	frozen   atomic.Bool
	onSignal func(execenv.Signal) error
}

func (p *deadlineProc) Signal(signal execenv.Signal) error {
	p.signals <- signal
	if p.frozen.Load() {
		return execenv.Error("signal", execenv.ErrFrozen)
	}
	if p.onSignal != nil {
		if err := p.onSignal(signal); err != nil {
			return err
		}
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

func TestDeadlineReattachDuringFreezeCancelsGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, DetachGrace: time.Second}); err != nil {
			t.Fatal(err)
		}
		m.Freeze()
		time.Sleep(2 * time.Second)
		a, err := m.Attach("p", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		m.Thaw()
		time.Sleep(killDelay)
		synctest.Wait()
		expectNoDeadlineSignal(t, p)
	})
}

func TestDeadlineReattachRacingGraceDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		p.onSignal = func(execenv.Signal) error {
			<-gate
			return execenv.ErrUnavailable
		}
		if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, DetachGrace: time.Second}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalHangup)
		if a, err := m.Attach("p", nil); !errors.Is(err, execenv.ErrBusy) {
			if a != nil {
				_ = a.Close()
			}
			t.Fatalf("attach with HUP in flight = %v, want ErrBusy", err)
		}
		release()
		synctest.Wait()
		a, err := m.Attach("p", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		time.Sleep(killDelay)
		synctest.Wait()
		expectNoDeadlineSignal(t, p)
	})
}

func TestDeadlineThawGatesInputUntilDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		p.onSignal = func(execenv.Signal) error { <-gate; return nil }
		if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, Timeout: time.Second}); err != nil {
			t.Fatal(err)
		}
		m.Freeze()
		time.Sleep(2 * time.Second)
		thawed := make(chan struct{})
		go func() { m.Thaw(); close(thawed) }()
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalTerminate)
		if _, err := m.WriteInput("p", 0, []byte("blocked")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("input during thaw = %v", err)
		}
		a, err := m.Attach("p", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if _, err := p.w.Write([]byte("output during delivery")); err != nil {
			t.Fatal(err)
		}
		if out, err := a.Next(t.Context()); err != nil || string(out.Data) != "output during delivery" {
			t.Fatalf("output during thaw = %q, %v", out.Data, err)
		}
		release()
		<-thawed
		if n, err := m.WriteInput("p", 0, []byte("ready")); err != nil || n != 5 {
			t.Fatalf("input after thaw = %d, %v", n, err)
		}
	})
}

func TestDeadlineRetriesFailedKillWhileRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		first := true
		p.onSignal = func(sig execenv.Signal) error {
			if sig == execenv.SignalKill && first {
				first = false
				return execenv.ErrUnavailable
			}
			return nil
		}
		if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, Timeout: time.Second}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalTerminate)
		m.Freeze()
		time.Sleep(killDelay)
		m.Thaw()
		expectDeadlineSignal(t, p, execenv.SignalKill)
		if _, err := m.WriteInput("p", 0, []byte("ready")); err != nil {
			t.Fatalf("input after first KILL attempt = %v", err)
		}
		time.Sleep(deadlineRetry)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalKill)
	})
}

func TestDeadlineSuspendPausesDetachGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old, oldProc := newDeadlineProc(t)
		spec := execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, DetachGrace: time.Second}
		info, err := old.Start(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second / 2)
		old.Freeze()
		saved := old.SuspendDeadlines()["p"]
		oldProc.exit()
		time.Sleep(time.Hour)

		m, p := newDeadlineProc(t)
		m.Freeze()
		if err := m.Adopt(Adopted{Spec: spec, Info: info, Deadlines: &saved}, p); err != nil {
			t.Fatal(err)
		}
		m.Thaw()
		expectNoDeadlineSignal(t, p)
		time.Sleep(time.Second / 2)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalHangup)
	})
}

func TestDeadlineThawGateExpiresWhileSignalIsBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		p.onSignal = func(execenv.Signal) error { <-gate; return execenv.ErrUnavailable }
		if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, Timeout: time.Second}); err != nil {
			t.Fatal(err)
		}
		m.Freeze()
		time.Sleep(time.Second)
		thawed := make(chan struct{})
		go func() { m.Thaw(); close(thawed) }()
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalTerminate)
		if _, err := m.WriteInput("p", 0, []byte("blocked")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("input while first attempt pending = %v", err)
		}
		time.Sleep(2 * time.Second)
		<-thawed
		if _, err := m.WriteInput("p", 0, []byte("ready")); err != nil {
			t.Fatalf("input after thaw gate expired = %v", err)
		}
		release()
		synctest.Wait()
		time.Sleep(deadlineRetry)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalTerminate)
		m.Freeze()
	})
}

func TestPlainFreezePausesDetachGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, p := newDeadlineProc(t)
		spec := execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, DetachGrace: time.Second, Timeout: time.Second}
		if _, err := m.Start(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second / 2)
		m.Freeze()
		time.Sleep(time.Hour)
		m.Thaw()
		expectDeadlineSignal(t, p, execenv.SignalTerminate)
		expectNoDeadlineSignal(t, p)
		time.Sleep(time.Second / 2)
		synctest.Wait()
		expectDeadlineSignal(t, p, execenv.SignalHangup)
	})
}
