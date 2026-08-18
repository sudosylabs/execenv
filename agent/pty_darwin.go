//go:build darwin

package agent

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (master, pts *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := ioctlNoPtr(fd, unix.TIOCPTYGRANT); err != nil {
		_ = unix.Close(fd)
		return nil, nil, err
	}
	if err := ioctlNoPtr(fd, unix.TIOCPTYUNLK); err != nil {
		_ = unix.Close(fd)
		return nil, nil, err
	}
	var buf [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		_ = unix.Close(fd)
		return nil, nil, errno
	}
	name := unix.ByteSliceToString(buf[:])
	slave, err := os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = unix.Close(fd)
		return nil, nil, err
	}
	return os.NewFile(uintptr(fd), "ptmx"), slave, nil
}

func ioctlNoPtr(fd int, req uint) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
