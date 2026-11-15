//go:build isolation && linux

package isolated

import (
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// TestLiveLoadFromOneTemplate starts many grants from one template at
// once and reports how long they took. It is a benchmark for an operator,
// run by hand: EXECENV_LOAD_GRANTS sets how many (100 is the reference),
// and the host needs room for all of them.
func TestLiveLoadFromOneTemplate(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("EXECENV_LOAD_GRANTS"))
	if n <= 0 {
		t.Skip("set EXECENV_LOAD_GRANTS to run the load benchmark")
	}
	host := liveShapedHost(t, n)
	var mu sync.Mutex
	var took []time.Duration
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			_, err := host.Ensure(t.Context(), execenv.Spec{ID: execenv.ID("load-" + strconv.Itoa(i)), Image: "default"})
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			took = append(took, time.Since(started))
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Ensure: %v", err)
	}
	if len(took) == 0 {
		return
	}
	slices.Sort(took)
	t.Logf("%d grants from one template: p50 %v, p99 %v, max %v", len(took), took[len(took)/2], took[len(took)*99/100], took[len(took)-1])
}
