//go:build linux

package isolated

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

func TestProcessSuspensionAcknowledgement(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := signalProcess(ctx, cmd, true); err != nil {
		t.Fatal(err)
	}
	if stopped, err := processState(cmd.Process.Pid, true); err != nil || !stopped {
		t.Fatalf("not stopped: %v %v", stopped, err)
	}
	if err := signalProcess(ctx, cmd, false); err != nil {
		t.Fatal(err)
	}
	if running, err := processState(cmd.Process.Pid, false); err != nil || !running {
		t.Fatalf("not resumed: %v %v", running, err)
	}
	if err := signalProcess(ctx, nil, true); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("missing process acknowledged: %v", err)
	}
}

func TestStopAcknowledgesReapedProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	p := &processInstance{cmd: cmd, done: done}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("destruction acknowledged before exit")
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("stop replay: %v", err)
	}
}
