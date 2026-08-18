package bake

import (
	_ "embed"
	"strconv"

	"github.com/sudosylabs/execenv"
)

// Default catalog pin. The source is a published universal-class
// container. Bake copies a caller-supplied kernel; it does not compile one.
const (
	DefaultID         = "default"
	DefaultSource     = "mcr.microsoft.com/devcontainers/universal:linux"
	DefaultKernelFile = "vmlinux"
	DefaultRootfsFile = "rootfs.ext4"
)

//go:embed guest.sh
var guestInit string

// Request is one bake. Source is a container reference. Dockerfile, when
// set, is an extra catalog id built from the operator's file instead.
type Request struct {
	ID         execenv.Image
	Source     string
	Dockerfile string
	Kernel     string
	Agent      string
	OutDir     string
	Size       int64
}

// Result is the files Ready and host.json consume.
type Result struct {
	ID      execenv.Image
	Kernel  string
	Rootfs  string
	Hash    string
	Source  string
	Version string
}

// ParseSize accepts bytes or a decimal integer with a K/M/G suffix.
// Empty means "choose from the exported tree plus 64MiB headroom".
func ParseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1 << 10
		s = s[:len(s)-1]
	case 'M', 'm':
		mult = 1 << 20
		s = s[:len(s)-1]
	case 'G', 'g':
		mult = 1 << 30
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, execenv.ErrInvalid
	}
	return n * mult, nil
}

func applyDefaults(req Request) Request {
	if req.ID == "" {
		req.ID = DefaultID
	}
	if req.Dockerfile == "" && req.Source == "" {
		req.Source = DefaultSource
	}
	return req
}
