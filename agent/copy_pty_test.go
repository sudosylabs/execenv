package agent

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestPtyEndFrozenWinsOverHangup(t *testing.T) {
	t.Parallel()
	eio := &os.PathError{Op: "read", Path: "ptmx", Err: syscall.EIO}
	if got := ptyEnd(true, eio); !errors.Is(got, execenv.ErrFrozen) {
		t.Fatalf("ptyEnd(frozen, EIO) = %v, want ErrFrozen", got)
	}
	if got := ptyEnd(false, eio); got != nil {
		t.Fatalf("ptyEnd(EIO) = %v, want a clean hangup", got)
	}
	if got := ptyEnd(false, io.EOF); got != nil {
		t.Fatalf("ptyEnd(EOF) = %v, want a clean hangup", got)
	}
	if got := ptyEnd(false, execenv.ErrBusy); !errors.Is(got, execenv.ErrBusy) {
		t.Fatalf("ptyEnd(ErrBusy) = %v, want ErrBusy", got)
	}
	if !errors.Is(eio, syscall.EIO) {
		t.Fatal("PathError EIO must match syscall.EIO")
	}
}
