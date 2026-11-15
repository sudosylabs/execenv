package agent_test

import (
	"testing"
	"time"

	"github.com/sudosylabs/execenv/agent"
)

// A host that dials while an old link is still open takes over: the old
// link ends, the new one is served.
func TestNewLinkTakesOverFromAnOldOne(t *testing.T) {
	a, err := agent.New(agent.Config{Home: t.TempDir(), Barrier: quiescent{}, Processes: newFakeProcesses()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	old, _ := link(t, a)
	fresh, _ := link(t, a)
	select {
	case <-old.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the old link was not ended")
	}
	if err := fresh.Verify(t.Context()); err != nil {
		t.Fatalf("new link: %v", err)
	}
}
