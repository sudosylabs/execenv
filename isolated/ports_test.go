package isolated

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

func TestPortConformance(t *testing.T) {
	host := testHost(t, func() error { return nil }, &recordingLauncher{})
	execenvtest.RunPorts(t, host, execenvtest.LocalPortListener(nil))
}

func TestPortConnectionEndsOnLinkLoss(t *testing.T) {
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "ports", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	port, _ := execenvtest.LocalPortListener(nil)(t, env, fence, "127.0.0.1", "hold")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := env.ConnectPort(ctx, fence, port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	launch.mu.Lock()
	inst := launch.instances[0]
	launch.mu.Unlock()
	inst.dropLink()
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, execenv.ErrLost) {
		t.Fatalf("Read() = %v, want ErrLost", err)
	}
}
