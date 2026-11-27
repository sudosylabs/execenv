// Package lifetime ends grants that outlive their wall timeout or sit idle
// past their idle timeout.
//
// A grant is busy while anything holds it (an attached terminal) and for
// the idle timeout after its last activity. The timer reports when a
// deadline is due; it never acts on the grant itself.
package lifetime

import (
	"sync"
	"time"
)

// Reason is why a timer expired.
type Reason uint8

const (
	// Wall is the grant's wall timeout.
	Wall Reason = iota + 1
	// Idle is the grant's idle timeout.
	Idle
)

// minIdleRetry bounds polling after an already-due expiry. The initial
// deadline and deadlines reset by activity still use the grant timeout.
const minIdleRetry = 100 * time.Millisecond

// Timer watches one grant.
type Timer struct {
	idle   time.Duration
	expire func(Reason)

	mu       sync.Mutex
	last     time.Time
	holds    int
	stopped  bool
	checking bool
	wall     *time.Timer
	idleT    *time.Timer
}

// New creates an inert watcher. Start it after publishing the timer to
// the code that handles its callbacks.
func New(expire func(Reason)) *Timer {
	return &Timer{expire: expire}
}

// Start begins watching. A zero wall or idle disables that timeout.
func (t *Timer) Start(wall, idle time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	if wall > 0 {
		t.wall = time.AfterFunc(wall, func() { t.fire(Wall) })
	}
	if idle > 0 {
		t.idle, t.last = idle, time.Now()
		t.idleT = time.AfterFunc(idle, t.checkIdle)
	}
}

func (t *Timer) fire(r Reason) {
	t.mu.Lock()
	stopped := t.stopped
	t.mu.Unlock()
	if !stopped {
		t.expire(r)
	}
}

func (t *Timer) checkIdle() {
	t.mu.Lock()
	if t.stopped || t.holds > 0 || t.checking {
		t.mu.Unlock()
		return
	}
	if rest := t.idle - time.Since(t.last); rest > 0 {
		t.idleT.Reset(rest)
		t.mu.Unlock()
		return
	}
	t.checking = true
	t.mu.Unlock()
	t.expire(Idle)
	t.mu.Lock()
	t.checking = false
	if !t.stopped && t.holds == 0 {
		rest := t.idle - time.Since(t.last)
		if rest <= 0 {
			rest = max(t.idle, minIdleRetry)
		}
		t.idleT.Reset(rest)
	}
	t.mu.Unlock()
}

// IdleDue rechecks the deadline when the caller acquires control of the grant.
func (t *Timer) IdleDue() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.stopped && t.idle > 0 && t.holds == 0 && time.Since(t.last) >= t.idle
}

// Touch marks activity now.
func (t *Timer) Touch() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
}

// ArmIdle starts a fresh idle period, including for an adopted suspension.
func (t *Timer) ArmIdle(idle time.Duration) {
	if t == nil || idle <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.idle, t.last = idle, time.Now()
	t.resetIdleLocked()
}

func (t *Timer) resetIdleLocked() {
	if t.stopped || t.idle <= 0 || t.holds > 0 || t.checking {
		return
	}
	if t.idleT == nil {
		t.idleT = time.AfterFunc(t.idle, t.checkIdle)
	} else {
		t.idleT.Reset(t.idle)
	}
}

// Hold keeps the grant busy until the returned release runs. Release is
// safe to call more than once.
func (t *Timer) Hold() func() {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	t.holds++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			t.holds--
			t.last = time.Now()
			t.resetIdleLocked()
			t.mu.Unlock()
		})
	}
}

// Stop ends watching. No callback runs after Stop returns, except one
// already running.
func (t *Timer) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.stopped = true
	if t.wall != nil {
		t.wall.Stop()
	}
	if t.idleT != nil {
		t.idleT.Stop()
	}
	t.mu.Unlock()
}
