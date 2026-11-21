package portconn

import (
	"math"
	"time"

	"github.com/sudosylabs/execenv"
)

// ChunkSize bounds relay frames and how much data a throttled connection
// pulls from the guest at a time.
const ChunkSize = 32 << 10

type bucket struct {
	burst  float64
	credit float64
	at     time.Time
}

type direction int

const (
	fromGuest direction = iota
	toGuest
)

// Full is checked under the grant's control gate before dialing.
func (s *Set) Full() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns) >= execenv.MaxPortConns
}

// SetLimit replaces both directional buckets and wakes waiting transfers.
// An unchanged configuration keeps the existing credit.
func (s *Set) SetLimit(limit execenv.TokenBucket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit == limit {
		return
	}
	s.limit = limit
	initial := bucket{burst: float64(limit.Burst), at: time.Now()}
	s.buckets = [2]bucket{initial, initial}
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// allow grants up to want bytes out of the shared grant bucket. Small
// rates still make progress with a zero burst. At higher rates, waiting
// for a batch avoids scheduling a timer for every byte.
func (c *held) allow(dir direction, want int) (int, error) {
	s := c.set
	for {
		s.mu.Lock()
		if c.cause != nil {
			err := c.cause
			s.mu.Unlock()
			return 0, err
		}
		if s.limit.Rate == 0 {
			s.mu.Unlock()
			return want, nil
		}
		rate := float64(s.limit.Rate)
		batch := max(1, min(ChunkSize, s.limit.Rate/20))
		b := &s.buckets[dir]
		now := time.Now()
		b.credit = min(float64(batch), b.credit+now.Sub(b.at).Seconds()*rate)
		b.at = now
		if n := int(min(float64(want), b.burst+b.credit)); n > 0 {
			spentBurst := min(b.burst, float64(n))
			b.burst -= spentBurst
			b.credit -= float64(n) - spentBurst
			s.mu.Unlock()
			return n, nil
		}
		target := float64(min(int64(want), batch))
		delay := time.Duration(math.Ceil((target - b.burst - b.credit) / rate * float64(time.Second)))
		changed := s.changed
		s.mu.Unlock()

		timer := time.NewTimer(delay)
		select {
		case <-c.done:
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}
