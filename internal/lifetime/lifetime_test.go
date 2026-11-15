package lifetime

import (
	"testing"
	"time"
)

func TestTimerExpiresAtTheWallTimeout(t *testing.T) {
	got := make(chan Reason, 1)
	timer := Start(20*time.Millisecond, 0, func(r Reason) { got <- r })
	defer timer.Stop()
	select {
	case r := <-got:
		if r != Wall {
			t.Fatalf("reason = %v, want Wall", r)
		}
	case <-time.After(time.Second):
		t.Fatal("wall timeout never fired")
	}
}

func TestTouchAndHoldPostponeIdle(t *testing.T) {
	got := make(chan Reason, 4)
	timer := Start(0, 60*time.Millisecond, func(r Reason) { got <- r })
	defer timer.Stop()
	release := timer.Hold()
	select {
	case <-got:
		t.Fatal("idle fired while held")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	release()
	for range 3 {
		time.Sleep(30 * time.Millisecond)
		timer.Touch()
	}
	select {
	case <-got:
		t.Fatal("idle fired despite touches")
	default:
	}
	select {
	case r := <-got:
		if r != Idle {
			t.Fatalf("reason = %v, want Idle", r)
		}
	case <-time.After(time.Second):
		t.Fatal("idle never fired")
	}
}

func TestStopSilencesTheTimer(t *testing.T) {
	got := make(chan Reason, 1)
	timer := Start(20*time.Millisecond, 20*time.Millisecond, func(r Reason) { got <- r })
	timer.Stop()
	select {
	case <-got:
		t.Fatal("stopped timer fired")
	case <-time.After(100 * time.Millisecond):
	}
	var none *Timer
	none.Stop()
	none.Touch()
	none.Hold()()
}
