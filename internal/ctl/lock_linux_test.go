package ctl

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestManagerLockReleasesWithAnOpenDuplicate(t *testing.T) {
	opts := testOpts(t, t.TempDir())
	lock, err := opts.lockConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	duplicate, err := unix.FcntlInt(lock.file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(duplicate)

	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := opts.lockConfig()
	if err != nil {
		t.Fatalf("reacquire the released lock: %v", err)
	}
	defer next.Close()
}
