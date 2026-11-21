//go:build !linux

package agent

import (
	"context"

	"github.com/sudosylabs/execenv"
)

func listeningPorts(context.Context, func(int) string) ([]execenv.ListeningPort, error) {
	return nil, execenv.ErrUnavailable
}
