package lifetime

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestTimerExpiresAtTheWallTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := make(chan Reason, 1)
		timer := New(func(r Reason) { got <- r })
		timer.Start(time.Minute, 0)
		defer timer.Stop()
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case r := <-got:
			if r != Wall {
				t.Fatalf("reason = %v, want Wall", r)
			}
		default:
			t.Fatal("wall timeout never fired")
		}
	})
}

func TestTouchAndHoldPostponeIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := make(chan Reason, 4)
		timer := New(func(r Reason) { got <- r })
		timer.Start(0, time.Minute)
		defer timer.Stop()
		release := timer.Hold()
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if timer.IdleDue() || len(got) != 0 {
			t.Fatal("idle fired while held")
		}
		release()
		release()
		for range 3 {
			time.Sleep(30 * time.Second)
			timer.Touch()
		}
		synctest.Wait()
		if timer.IdleDue() || len(got) != 0 {
			t.Fatal("idle fired despite touches")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case r := <-got:
			if r != Idle {
				t.Fatalf("reason = %v, want Idle", r)
			}
		default:
			t.Fatal("idle never fired")
		}
	})
}

func TestIdleDecisionExpiresWhenAConnectionTakesAHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		due := make(chan struct{})
		check := make(chan struct{})
		eligible := make(chan bool, 1)
		var timer *Timer
		timer = New(func(Reason) {
			close(due)
			<-check
			eligible <- timer.IdleDue()
		})
		timer.Start(0, time.Minute)
		defer timer.Stop()
		<-due
		release := timer.Hold()
		defer release()
		close(check)
		if <-eligible {
			t.Fatal("a stale idle decision survived a new hold")
		}
	})
}

func TestStopSilencesTheTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := make(chan Reason, 2)
		timer := New(func(r Reason) { got <- r })
		timer.Start(time.Minute, time.Minute)
		timer.Stop()
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if len(got) != 0 {
			t.Fatal("stopped timer fired")
		}
	})
	var none *Timer
	none.Stop()
	none.Touch()
	none.Hold()()
}

func TestIdleDoesNotQueueCallbacksBehindAnOutstandingExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := make(chan Reason, 8)
		unblock := make(chan struct{})
		timer := New(func(r Reason) {
			got <- r
			<-unblock
		})
		timer.Start(0, time.Minute)
		defer close(unblock)
		defer timer.Stop()
		<-got
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if len(got) != 0 {
			t.Fatal("new callbacks accumulated while the first one was blocked")
		}
	})
}

func TestVeryShortIdleTimeoutDoesNotPollAtTheInitialRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Microsecond
		fired := make(chan Reason, 32)
		timer := New(func(reason Reason) { fired <- reason })
		timer.Start(0, idle)
		defer timer.Stop()
		time.Sleep(10 * idle)
		synctest.Wait()
		if got := len(fired); got != 1 {
			t.Fatalf("callbacks before the retry delay = %d, want one", got)
		}
		time.Sleep(minIdleRetry)
		synctest.Wait()
		if got := len(fired); got != 2 {
			t.Fatalf("callbacks after the retry delay = %d, want two", got)
		}
	})
}
