// Package lifetime ends grants that outlive their wall timeout or sit idle
// past their idle timeout.
//
// A grant is busy while anything holds it (an attached terminal) and for
// the idle timeout after its last touch (any call on it). The timer calls
// back at most once per expiry; it never acts on the grant itself.
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

// Timer watches one grant.
type Timer struct {
	idle   time.Duration
	expire func(Reason)

	mu      sync.Mutex
	last    time.Time
	holds   int
	stopped bool
	wall    *time.Timer
	idleT   *time.Timer
}

// Start begins watching. A zero wall or idle disables that timeout. expire
// runs on its own goroutine.
func Start(wall, idle time.Duration, expire func(Reason)) *Timer {
	t := &Timer{idle: idle, expire: expire, last: time.Now()}
	if wall > 0 {
		t.wall = time.AfterFunc(wall, func() { t.fire(Wall) })
	}
	if idle > 0 {
		t.idleT = time.AfterFunc(idle, t.checkIdle)
	}
	return t
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
	if t.stopped {
		t.mu.Unlock()
		return
	}
	if t.holds > 0 {
		t.idleT.Reset(t.idle)
		t.mu.Unlock()
		return
	}
	if rest := t.idle - time.Since(t.last); rest > 0 {
		t.idleT.Reset(rest)
		t.mu.Unlock()
		return
	}
	// Expired. A later touch starts a new idle period.
	t.last = time.Now()
	t.idleT.Reset(t.idle)
	t.mu.Unlock()
	t.expire(Idle)
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
