//go:build !linux

package workload

import "github.com/sudosylabs/execenv"

func RunInit([]string) error { return execenv.ErrUnavailable }
