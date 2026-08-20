package agent

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestPtyEndStatusFrozenWinsOverHangup(t *testing.T) {
	t.Parallel()
	eio := &os.PathError{Op: "read", Path: "ptmx", Err: syscall.EIO}
	if got := ptyEndStatus(true, eio); got != "frozen" {
		t.Fatalf("ptyEndStatus(frozen, EIO) = %q, want frozen", got)
	}
	if got := ptyEndStatus(false, eio); got != "" {
		t.Fatalf("ptyEndStatus(EIO) = %q, want empty hangup", got)
	}
	if got := ptyEndStatus(false, io.EOF); got != "" {
		t.Fatalf("ptyEndStatus(EOF) = %q, want empty hangup", got)
	}
	if got := ptyEndStatus(false, execenv.ErrBusy); got != "busy" {
		t.Fatalf("ptyEndStatus(ErrBusy) = %q, want busy", got)
	}
	if !errors.Is(eio, syscall.EIO) {
		t.Fatal("PathError EIO must match syscall.EIO")
	}
}
