//go:build linux

package agent

import (
	"encoding/binary"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"golang.org/x/sys/unix"
)

func procMessage(what uint32, fields ...uint32) []byte {
	order := binary.NativeEndian
	event := make([]byte, procEventHeader+4*len(fields))
	order.PutUint32(event[0:], what)
	for i, field := range fields {
		order.PutUint32(event[procEventHeader+4*i:], field)
	}
	message := make([]byte, unix.SizeofNlMsghdr+cnMsgBytes+len(event))
	order.PutUint32(message[0:], uint32(len(message)))
	order.PutUint32(message[16:], cnIdxProc)
	order.PutUint32(message[20:], cnValProc)
	order.PutUint16(message[32:], uint16(len(event)))
	copy(message[36:], event)
	for len(message)%4 != 0 {
		message = append(message, 0)
	}
	return message
}

func TestParseProcMessages(t *testing.T) {
	var data []byte
	data = append(data, procMessage(procEventNone, 0)...)
	data = append(data, procMessage(procEventFork, 9, 10, 11, 11)...)
	data = append(data, procMessage(procEventExec, 11, 11)...)
	data = append(data, procMessage(procEventExit, 11, 11, 3<<8, 17)...)
	data = append(data, procMessage(0x00000004, 11, 11)...)
	events, ack := parseProcMessages(data, time.Unix(1, 0))
	if ack != ackAccepted || len(events) != 3 {
		t.Fatalf("events %+v ack %v", events, ack)
	}
	if _, ack := parseProcMessages(procMessage(procEventNone, 1), time.Now()); ack != ackRefused {
		t.Fatalf("refused subscription: %v", ack)
	}
	if events[0].Kind != kernelFork || events[0].Parent != 10 || events[0].PID != 11 || events[0].TGID != 11 {
		t.Fatalf("fork: %+v", events[0])
	}
	if events[1].Kind != kernelExec || events[1].TGID != 11 || events[2].Kind != kernelExit || events[2].Status != 3<<8 {
		t.Fatalf("exec/exit: %+v", events[1:])
	}
	if events, _ := parseProcMessages(data[:20], time.Now()); len(events) != 0 {
		t.Fatal("truncated datagram decoded")
	}
}

// TestKernelProcessesRecordAChild reads this machine's own process events.
// It needs root and a kernel with process events, as the guest helper has.
func TestKernelProcessesRecordAChild(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the process events connector needs root")
	}
	group, err := procfs{}.cgroup(os.Getpid())
	if err != nil {
		t.Skipf("no unified hierarchy: %v", err)
	}
	reader, err := openConnector()
	if err != nil {
		t.Skipf("kernel lacks process events: %v", err)
	}
	dir := t.TempDir()
	source := &kernelProcesses{reader: reader, tracker: newTracker(procfs{}, group, -1, dir)}
	t.Cleanup(func() { _ = source.Close() })
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.2; exit 3", "probe")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	var started, exited *execenv.ProcessEvent
	timer := time.AfterFunc(10*time.Second, func() { _ = source.Close() })
	defer timer.Stop()
	for exited == nil {
		event, err := source.Next()
		if err != nil {
			t.Fatalf("no events for %d: %v", pid, err)
		}
		switch {
		case event.Kind == execenv.ProcessStarted && event.PID == pid:
			started = &event
		case event.Kind == execenv.ProcessExited && event.PID == pid && started != nil:
			exited = &event
		}
	}
	if !slices.Equal(started.Argv, []string{"/bin/sh", "-c", "sleep 0.2; exit 3", "probe"}) || started.Dir != dir || started.ParentPID != os.Getpid() {
		t.Fatalf("started: %+v", started)
	}
	if exited.ExitCode == nil || *exited.ExitCode != 3 {
		t.Fatalf("exited: %+v", exited)
	}
}
