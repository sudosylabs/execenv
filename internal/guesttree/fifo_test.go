//go:build linux || darwin

package guesttree

import "syscall"

func makeFIFO(name string) error { return syscall.Mkfifo(name, 0600) }
