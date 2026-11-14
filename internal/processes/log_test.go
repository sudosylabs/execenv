package processes

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

func started(pid int, args ...string) execenv.ProcessEvent {
	return execenv.ProcessEvent{Kind: execenv.ProcessStarted, PID: pid, ParentPID: 1, Argv: args, At: time.Unix(1, 0)}
}

func TestLogSequencesTrimsAndRefusesOldPositions(t *testing.T) {
	log := New()
	log.Append(started(10, "a"))
	log.Append(Exited(10, 3<<8, time.Unix(2, 0)))
	log.Append(Exited(11, 9, time.Unix(2, 0)))
	page, err := log.Page(0, 10)
	if err != nil || len(page.Events) != 3 || page.Next != 3 {
		t.Fatalf("page: %+v %v", page, err)
	}
	if page.Events[1].ExitCode == nil || *page.Events[1].ExitCode != 3 || page.Events[2].Signal != 9 || page.Events[2].ExitCode != nil {
		t.Fatalf("exit status: %+v", page.Events)
	}
	page.Events[0].Argv[0] = "changed"
	if again, _ := log.Page(0, 1); again.Events[0].Argv[0] != "a" {
		t.Fatal("page aliases retention")
	}
	if err := log.Acknowledge(2); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Page(1, 10); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("acknowledged position: %v", err)
	}
	if page, err := log.Page(2, 10); err != nil || len(page.Events) != 1 || page.Events[0].Sequence != 3 {
		t.Fatalf("after acknowledgement: %+v %v", page, err)
	}
	if page, err := log.Page(3, 10); err != nil || len(page.Events) != 0 || page.Next != 3 {
		t.Fatalf("empty page: %+v %v", page, err)
	}
	if _, err := log.Page(4, 10); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("future position: %v", err)
	}
	if err := log.Acknowledge(4); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("future acknowledgement: %v", err)
	}
	if err := log.Acknowledge(1); err != nil {
		t.Fatalf("older acknowledgement: %v", err)
	}
	for _, limit := range []int{0, execenv.MaxProcessPage + 1} {
		if _, err := log.Page(2, limit); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
}

func TestLogOverflowCountsInsteadOfEvicting(t *testing.T) {
	log := New()
	log.maxEvents = 4
	for pid := 1; pid <= 7; pid++ {
		log.Append(started(pid, "x"))
	}
	page, _ := log.Page(0, 10)
	if len(page.Events) != 4 || page.Events[3].PID != 4 {
		t.Fatalf("retained: %+v", page.Events)
	}
	// Space returns; the next event follows one marker counting the loss.
	if err := log.Acknowledge(2); err != nil {
		t.Fatal(err)
	}
	log.Append(started(8, "x"))
	page, _ = log.Page(4, 10)
	if len(page.Events) != 2 || page.Events[0].Kind != execenv.ProcessDropped || page.Events[0].Dropped != 3 || page.Events[0].Sequence != 5 || page.Events[1].PID != 8 {
		t.Fatalf("after space returned: %+v", page.Events)
	}
}

func TestLogFlushesLossOnAcknowledgement(t *testing.T) {
	log := New()
	log.maxEvents = 2
	for pid := 1; pid <= 5; pid++ {
		log.Append(started(pid, "x"))
	}
	if err := log.Acknowledge(2); err != nil {
		t.Fatal(err)
	}
	page, _ := log.Page(2, 10)
	if len(page.Events) != 1 || page.Events[0].Kind != execenv.ProcessDropped || page.Events[0].Dropped != 3 {
		t.Fatalf("loss not reported: %+v", page.Events)
	}
}

func TestLogByteBoundAndInvalidEvents(t *testing.T) {
	log := New()
	log.maxBytes = 200
	log.Append(started(1, strings.Repeat("a", 100)))
	log.Append(started(2, strings.Repeat("b", 100)))
	log.Append(execenv.ProcessEvent{Kind: execenv.ProcessExited, PID: 3})
	log.Lost(2)
	if err := log.Acknowledge(1); err != nil {
		t.Fatal(err)
	}
	page, _ := log.Page(1, 10)
	if len(page.Events) != 1 || page.Events[0].Dropped != 4 {
		t.Fatalf("bytes, invalid and producer loss: %+v", page.Events)
	}
}

func TestArgvBounds(t *testing.T) {
	many := make([]string, 70)
	for i := range many {
		many[i] = "a"
	}
	if argv, cut := Argv(many); len(argv) != execenv.MaxProcessArgs || !cut {
		t.Fatalf("argument count: %d %v", len(argv), cut)
	}
	long := []string{"cmd", strings.Repeat("é", 600)}
	argv, cut := Argv(long)
	total := len(argv[0]) + len(argv[1])
	if !cut || total > execenv.MaxProcessArgvBytes || total < execenv.MaxProcessArgvBytes-1 || !strings.HasPrefix(long[1], argv[1]) {
		t.Fatalf("argument bytes: %d %v", total, cut)
	}
	if argv, cut := Argv([]string{"a\xffb"}); cut || argv[0] != "a�b" {
		t.Fatalf("invalid UTF-8: %q", argv)
	}
	if argv, cut := Argv([]string{"sh", "-l"}); cut || len(argv) != 2 {
		t.Fatal("short command line cut")
	}
}
