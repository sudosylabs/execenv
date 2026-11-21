package procs

import (
	"errors"
	"time"

	"github.com/sudosylabs/execenv"
)

type deadlineKind int

const (
	timeoutDeadline deadlineKind = iota
	graceDeadline
	timeoutKill
	graceKill
	deadlineCount
)

const (
	deadlineRetry = 100 * time.Millisecond
	thawGrace     = 2 * time.Second
)

var deadlineSignals = [deadlineCount]execenv.Signal{
	timeoutDeadline: execenv.SignalTerminate,
	graceDeadline:   execenv.SignalHangup,
	timeoutKill:     execenv.SignalKill,
	graceKill:       execenv.SignalKill,
}

func (k deadlineKind) detach() bool { return k == graceDeadline || k == graceKill }

// Deadlines holds absolute process deadlines across a guest suspend.
type Deadlines struct {
	at       [deadlineCount]time.Time
	pausedAt time.Time
}

// SuspendDeadlines snapshots the deadlines before dropping host handles.
// Detach grace is paused on disk. An attachment still held at suspend
// gets a full grace on resume. Timeout and its KILL count wall-clock time.
func (m *Manager) SuspendDeadlines() map[string]Deadlines {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Deadlines)
	now := m.now()
	for tag, p := range m.procs {
		if p.backend == nil || p.exited {
			continue
		}
		d := p.deadlines
		d.pause(now)
		if p.attached > 0 && p.spec.DetachGrace > 0 {
			d.at[graceDeadline] = d.pausedAt.Add(p.spec.DetachGrace)
		}
		out[tag] = d
	}
	return out
}

func (d *Deadlines) pause(now time.Time) {
	if d.pausedAt.IsZero() {
		d.pausedAt = now
	}
}

func (d Deadlines) resume(now time.Time) Deadlines {
	if !d.pausedAt.IsZero() {
		for _, kind := range []deadlineKind{graceDeadline, graceKill} {
			if !d.at[kind].IsZero() {
				d.at[kind] = d.at[kind].Add(now.Sub(d.pausedAt))
			}
		}
		d.pausedAt = time.Time{}
	}
	return d
}

func (p *proc) armGraceLocked() {
	if p.spec.DetachGrace > 0 && p.attached == 0 && !p.exited && p.deadlines.at[graceDeadline].IsZero() {
		p.deadlines.at[graceDeadline] = p.detachNowLocked().Add(p.spec.DetachGrace)
	}
}

func (p *proc) detachNowLocked() time.Time {
	if !p.deadlines.pausedAt.IsZero() {
		return p.deadlines.pausedAt
	}
	return p.m.now()
}

func (m *Manager) closeThawGateLocked() {
	if m.thawDone != nil {
		close(m.thawDone)
		m.thawDone = nil
	}
	m.thawUntil = time.Time{}
}

func (p *proc) clearGraceLocked() {
	for _, kind := range []deadlineKind{graceDeadline, graceKill} {
		p.clearDeadlineLocked(kind)
	}
}

func (p *proc) markPendingLocked(now time.Time) {
	if p.backend == nil || p.exited || p.m.ended != nil {
		return
	}
	for kind, at := range p.deadlines.at {
		if !at.IsZero() && !at.After(now) && !p.thawPending[kind] {
			p.thawPending[kind] = true
			p.m.pendingThaw++
		}
	}
}

func (p *proc) clearPendingLocked() {
	for kind := range p.thawPending {
		p.clearDeadlineLocked(deadlineKind(kind))
	}
}

func (p *proc) clearDeadlineLocked(kind deadlineKind) {
	p.deadlines.at[kind], p.retryAt[kind] = time.Time{}, time.Time{}
	p.finishAttemptLocked(kind)
}

func (p *proc) finishAttemptLocked(kind deadlineKind) {
	if p.thawPending[kind] {
		p.thawPending[kind] = false
		p.m.pendingThaw--
		if p.m.pendingThaw == 0 {
			p.m.closeThawGateLocked()
		}
	}
}

func (p *proc) stopDeadlineLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

func (p *proc) canDeliverLocked() bool {
	return p.backend != nil && !p.exited && p.m.ended == nil && !p.m.frozen && !p.blocked
}

func (p *proc) nextDeadlineLocked() (deadlineKind, time.Time) {
	var next time.Time
	var kind deadlineKind
	for i, when := range p.deadlines.at {
		k := deadlineKind(i)
		if when.IsZero() || (k.detach() && p.attached > 0) {
			continue
		}
		if retry := p.retryAt[k]; retry.After(when) {
			when = retry
		}
		if next.IsZero() || when.Before(next) {
			kind, next = k, when
		}
	}
	return kind, next
}

func (p *proc) scheduleDeadlineLocked() {
	p.stopDeadlineLocked()
	if !p.canDeliverLocked() || p.inFlight {
		return
	}
	if _, next := p.nextDeadlineLocked(); !next.IsZero() {
		p.timer = time.AfterFunc(next.Sub(p.m.now()), p.deliverDeadlines)
	}
}

// deliverDeadlines claims a signal under the mutex and sends it outside.
// Deadline workers never block the manager's lock on backend I/O.
func (p *proc) deliverDeadlines() {
	m := p.m
	for {
		m.mu.Lock()
		if !p.canDeliverLocked() {
			m.mu.Unlock()
			return
		}
		if p.inFlight {
			m.mu.Unlock()
			return
		}
		kind, next := p.nextDeadlineLocked()
		if next.IsZero() || next.After(m.now()) {
			p.scheduleDeadlineLocked()
			m.mu.Unlock()
			return
		}
		p.stopDeadlineLocked()
		p.inFlight = true
		p.flightKind = kind
		epoch := m.thawEpoch
		m.mu.Unlock()

		err := p.backend.Signal(deadlineSignals[kind])

		m.mu.Lock()
		p.recordDeadlineLocked(kind, epoch, err)
		p.finishAttemptLocked(kind)
		p.inFlight = false
		m.mu.Unlock()
	}
}

func (p *proc) recordDeadlineLocked(kind deadlineKind, epoch uint64, err error) {
	if p.exited || p.m.ended != nil {
		return
	}
	now := p.m.now()
	switch {
	case err == nil:
		p.clearDeadlineLocked(kind)
		if kind == timeoutDeadline || kind == graceDeadline {
			kill := timeoutKill
			if kind == graceDeadline {
				kill = graceKill
				now = p.detachNowLocked()
			}
			if p.deadlines.at[kill].IsZero() {
				p.deadlines.at[kill] = now.Add(killDelay)
			}
		}
	case errors.Is(err, ErrProcessExited) || errors.Is(err, execenv.ErrNotFound):
		p.clearPendingLocked()
	case epoch != p.m.thawEpoch:
		// The signal crossed a reopen. Recheck immediately on the new link.
	case errors.Is(err, execenv.ErrFrozen):
		p.blocked = true
	default:
		p.retryAt[kind] = now.Add(deadlineRetry)
	}
}
