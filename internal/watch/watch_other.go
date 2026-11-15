//go:build !linux

package watch

import "github.com/sudosylabs/execenv"

// Watcher is unavailable off Linux; the journal walks on every Observe.
type Watcher struct{}

// New fails off Linux.
func New(string, func(int) string) (*Watcher, error) { return nil, execenv.ErrUnavailable }

func (*Watcher) Take() ([]string, bool)     { return nil, true }
func (*Watcher) Writers() map[string]string { return nil }
func (*Watcher) Close() error               { return nil }
