package execenv

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateProcessEvent(t *testing.T) {
	zero, big := 0, 256
	valid := []ProcessEvent{
		{Sequence: 1, Kind: ProcessStarted, PID: 2, ParentPID: 1, Argv: []string{"sh", "-l"}, Dir: GuestWorkspace},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, Unread: true},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, Argv: []string{strings.Repeat("a", MaxProcessArgvBytes)}, Truncated: true},
		{Sequence: 1, Kind: ProcessExited, PID: 2, ExitCode: &zero},
		{Sequence: 1, Kind: ProcessExited, PID: 2, Signal: 9},
		{Sequence: 1, Kind: ProcessDropped, Dropped: 3},
	}
	for _, event := range valid {
		if err := ValidateProcessEvent(event); err != nil {
			t.Fatalf("%+v: %v", event, err)
		}
	}
	invalid := []ProcessEvent{
		{Kind: ProcessStarted, PID: 2},
		{Sequence: 1, Kind: "forked", PID: 2},
		{Sequence: 1, Kind: ProcessStarted},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, Unread: true, Argv: []string{"x"}},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, Argv: make([]string, MaxProcessArgs+1)},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, Argv: []string{strings.Repeat("a", MaxProcessArgvBytes+1)}},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, Argv: []string{"\xff"}},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, Dir: "relative"},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, Dir: "/a/../b"},
		{Sequence: 1, Kind: ProcessStarted, PID: 2, ExitCode: &zero},
		{Sequence: 1, Kind: ProcessExited, PID: 2},
		{Sequence: 1, Kind: ProcessExited, PID: 2, ExitCode: &zero, Signal: 9},
		{Sequence: 1, Kind: ProcessExited, PID: 2, ExitCode: &big},
		{Sequence: 1, Kind: ProcessExited, PID: 2, Signal: 9, Argv: []string{"x"}},
		{Sequence: 1, Kind: ProcessDropped},
		{Sequence: 1, Kind: ProcessDropped, Dropped: 1, PID: 2},
	}
	for _, event := range invalid {
		if err := ValidateProcessEvent(event); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v accepted: %v", event, err)
		}
	}
}
