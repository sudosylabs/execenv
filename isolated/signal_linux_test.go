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
	pid := cmd.Process.Pid
	if err := signalProcess(ctx, pid, true); err != nil {
		t.Fatal(err)
	}
	if stopped, err := processState(pid, true); err != nil || !stopped {
		t.Fatalf("not stopped: %v %v", stopped, err)
	}
	if err := signalProcess(ctx, pid, false); err != nil {
		t.Fatal(err)
	}
	if running, err := processState(pid, false); err != nil || !running {
		t.Fatalf("not resumed: %v %v", running, err)
	}
	if err := signalProcess(ctx, 0, true); !errors.Is(err, execenv.ErrUnavailable) {
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
	start, _ := procStart(cmd.Process.Pid)
	p := &processInstance{pid: cmd.Process.Pid, start: start, done: done}
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

// A process that is not this host's child is still watched to its exit,
// and a reused pid is never taken for it.
func TestWatchExitFollowsAProcessByStartTime(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	pid := cmd.Process.Pid
	start, ok := procStart(pid)
	if !ok {
		t.Fatal("no start time")
	}
	if wrong := watchExit(pid, start+1); !isClosed(wrong) {
		t.Fatal("a different start time was taken for the same process")
	}
	done := watchExit(pid, start)
	if isClosed(done) {
		t.Fatal("running process reported exited")
	}
	_ = cmd.Process.Kill()
	<-reaped
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exit not observed")
	}
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}
