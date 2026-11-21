package procs

import (
	"errors"
	"time"

	"github.com/sudosylabs/execenv"
)

// Deadlines holds a process's absolute deadlines across a guest suspend.
type Deadlines struct {
	timeout time.Time
	grace   time.Time
	kill    time.Time
}

// SuspendDeadlines returns deadlines for the processes whose host handles
// are being dropped. An attachment that is still open detaches at this
// point, starting its grace unless one is already due.
func (m *Manager) SuspendDeadlines() map[string]Deadlines {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Deadlines)
	now := time.Now()
	for tag, p := range m.procs {
		if p.backend == nil || p.exited {
			continue
		}
		d := p.deadlines
		if p.attached > 0 && d.grace.IsZero() && p.spec.DetachGrace > 0 {
			d.grace = now.Add(p.spec.DetachGrace)
		}
		out[tag] = d
	}
	return out
}

func (p *proc) armGraceLocked() {
	if p.spec.DetachGrace > 0 && p.attached == 0 && !p.exited && p.deadlines.grace.IsZero() {
		p.deadlines.grace = time.Now().Add(p.spec.DetachGrace)
	}
}

func (p *proc) stopDeadlineLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

func (p *proc) scheduleDeadlineLocked() {
	p.stopDeadlineLocked()
	if p.backend == nil || p.exited || p.m.ended != nil || p.m.frozen || p.blocked {
		return
	}
	var next time.Time
	for _, at := range [...]time.Time{p.deadlines.timeout, p.deadlines.grace, p.deadlines.kill} {
		if !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	if !next.IsZero() {
		p.timer = time.AfterFunc(time.Until(next), func() {
			p.m.mu.Lock()
			defer p.m.mu.Unlock()
			p.deliverDeadlinesLocked(time.Now())
		})
	}
}

// deliverDeadlinesLocked is also called synchronously during Thaw and
// adoption. The manager gate stays held until all due signals are sent.
func (p *proc) deliverDeadlinesLocked(now time.Time) {
	if p.backend == nil || p.exited || p.m.ended != nil || p.m.frozen || p.blocked {
		return
	}
	p.deliverDeadlineLocked(now, &p.deadlines.timeout, execenv.SignalTerminate)
	p.deliverDeadlineLocked(now, &p.deadlines.grace, execenv.SignalHangup)
	p.deliverDeadlineLocked(now, &p.deadlines.kill, execenv.SignalKill)
	p.scheduleDeadlineLocked()
}

func (p *proc) deliverDeadlineLocked(now time.Time, at *time.Time, signal execenv.Signal) {
	if p.blocked || at.IsZero() || at.After(now) {
		return
	}
	if err := p.backend.Signal(signal); errors.Is(err, execenv.ErrFrozen) {
		p.blocked = true
		return
	}
	*at = time.Time{}
	if signal != execenv.SignalKill && p.deadlines.kill.IsZero() {
		p.deadlines.kill = time.Now().Add(killDelay)
	}
}
