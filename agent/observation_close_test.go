package agent

import (
	"github.com/sudosylabs/execenv"
	"sync"
	"testing"
)

func TestObservationDeliveryConcurrentWithClose(t *testing.T) {
	for range 1000 {
		observation := newClientObs(nil, 1, "")
		var wait sync.WaitGroup
		wait.Add(2)
		go func() { defer wait.Done(); observation.push(execenv.Event{Path: "file", Op: execenv.OpCreate}) }()
		go func() { defer wait.Done(); observation.fail(execenv.ErrClosed) }()
		wait.Wait()
	}
}
