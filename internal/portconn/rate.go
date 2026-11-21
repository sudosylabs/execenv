package portconn

import (
	"context"
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

// SetLimit replaces both directional buckets and wakes waiting transfers.
// An unchanged configuration keeps the existing credit.
func (s *Set) SetLimit(limit execenv.TokenBucket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed != nil && s.limit == limit {
		return
	}
	now := time.Now()
	for i := range s.buckets {
		b := &s.buckets[i]
		if s.changed == nil {
			b.burst = float64(limit.Burst)
		} else {
			b.refill(s.limit, now)
			b.burst = min(b.burst, float64(limit.Burst))
			b.credit = min(b.credit, float64(batchSize(limit)), float64(limit.Burst)-b.burst)
		}
		b.at = now
	}
	s.limit = limit
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// allow grants up to want bytes out of the shared grant bucket. Small
// rates still make progress with a zero burst. At higher rates, waiting
// for a batch avoids scheduling a timer for every byte.
func (c *Slot) allow(dir direction, want int) (int, error) {
	s := c.set
	for {
		s.mu.Lock()
		if err := context.Cause(c.ctx); err != nil {
			s.mu.Unlock()
			return 0, err
		}
		if s.limit.Rate == 0 {
			s.mu.Unlock()
			return want, nil
		}
		rate := float64(s.limit.Rate)
		batch := batchSize(s.limit)
		b := &s.buckets[dir]
		b.refill(s.limit, time.Now())
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
		case <-c.ctx.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func batchSize(limit execenv.TokenBucket) int64 {
	return max(1, min(ChunkSize, limit.Rate/20))
}

func (b *bucket) refill(limit execenv.TokenBucket, now time.Time) {
	if limit.Rate > 0 {
		b.credit = min(float64(batchSize(limit)), b.credit+now.Sub(b.at).Seconds()*float64(limit.Rate))
	}
	b.at = now
}
