package portconn

import (
	"testing"
	"testing/synctest"

	"github.com/sudosylabs/execenv"
)

func TestChangingLimitDoesNotRefillBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s Set
		s.SetLimit(execenv.TokenBucket{Rate: 100, Burst: 20})
		c, err := s.Reserve(t.Context(), func() {})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if n, err := c.allow(fromGuest, 12); err != nil || n != 12 {
			t.Fatalf("initial allowance = %d, %v", n, err)
		}
		s.SetLimit(execenv.TokenBucket{Rate: 200, Burst: 4})
		s.SetLimit(execenv.TokenBucket{})
		s.SetLimit(execenv.TokenBucket{Rate: 100, Burst: 20})
		if n, err := c.allow(fromGuest, 20); err != nil || n >= 20 {
			t.Fatalf("rate toggles refreshed the initial burst: %d, %v", n, err)
		}
	})
}
