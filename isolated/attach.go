package isolated

import "github.com/sudosylabs/execenv"

// attacher prepares Network for one grant. NetworkNone returns a nil
// attach (no NIC). Two adapters: production tap/filter, and a recorder
// for tests that must not create devices.
type attacher interface {
	Setup(id execenv.ID, dests []string) (*netAttach, error)
}

type allowAttacher struct{}

func (allowAttacher) Setup(id execenv.ID, dests []string) (*netAttach, error) {
	return setupAllowlist(id, dests)
}
