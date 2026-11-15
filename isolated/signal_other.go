//go:build !linux

package isolated

import (
	"context"

	"github.com/sudosylabs/execenv"
)

func signalProcess(context.Context, int, bool) error { return execenv.ErrUnavailable }

// procStart is unknown off Linux: no machine there is ever adopted.
func procStart(int) (uint64, bool) { return 0, false }

func watchExit(int, uint64) <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
