package observations

import (
	"context"
	"time"

	"github.com/sudosylabs/execenv"
)

// The pause between two looks while a wait finds nothing grows from
// firstPause to maxPause: a change made just after the call is seen at
// once, and an idle wait costs a few looks a second.
const (
	firstPause = 5 * time.Millisecond
	maxPause   = 100 * time.Millisecond
)

// Wait calls observe until it returns an observation or an error, or wait
// has passed, and returns what the last call did. Each call takes and
// releases whatever observe serializes on, so a freeze or projection never
// waits for the whole wait. A receive on wake looks again at once; nil
// only polls. The caller's context ending is its error.
func Wait(ctx context.Context, wait time.Duration, wake <-chan struct{}, observe func() (execenv.ObservationPage, error)) (execenv.ObservationPage, error) {
	deadline := time.Now().Add(wait)
	pause := firstPause
	for {
		page, err := observe()
		if err != nil || len(page.Observations) > 0 {
			return page, err
		}
		rest := time.Until(deadline)
		if rest <= 0 {
			return page, nil
		}
		timer := time.NewTimer(min(pause, rest))
		select {
		case <-ctx.Done():
			timer.Stop()
			return execenv.ObservationPage{}, ctx.Err()
		case <-wake:
			timer.Stop()
		case <-timer.C:
			pause = min(2*pause, maxPause)
		}
	}
}
