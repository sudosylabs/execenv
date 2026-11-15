//go:build !linux

package watch

import "github.com/sudosylabs/execenv"

// Watcher is unavailable off Linux; the journal walks on every Observe.
type Watcher struct{}

// New fails off Linux.
func New(string) (*Watcher, error) { return nil, execenv.ErrUnavailable }

func (*Watcher) Changed() bool { return true }
func (*Watcher) Close() error  { return nil }
