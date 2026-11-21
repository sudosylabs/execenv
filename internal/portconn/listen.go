package portconn

import (
	"cmp"
	"slices"

	"github.com/sudosylabs/execenv"
)

// AddressOrder returns the presentation order of supported addresses, or zero
// for other addresses. Prefer a listener reachable over IPv4.
func AddressOrder(addr string) int {
	switch addr {
	case "127.0.0.1":
		return 1
	case "0.0.0.0":
		return 2
	case "::1":
		return 3
	case "::":
		return 4
	default:
		return 0
	}
}

// List sorts sockets and keeps one per port, including when both families
// listen. If addresses match, keep a known tag before an empty one.
func List(ports []execenv.ListeningPort) []execenv.ListeningPort {
	slices.SortFunc(ports, func(a, b execenv.ListeningPort) int {
		if order := cmp.Compare(a.Port, b.Port); order != 0 {
			return order
		}
		if order := cmp.Compare(AddressOrder(a.Address), AddressOrder(b.Address)); order != 0 {
			return order
		}
		if a.Tag == "" && b.Tag != "" {
			return 1
		}
		if a.Tag != "" && b.Tag == "" {
			return -1
		}
		return cmp.Compare(a.Tag, b.Tag)
	})
	return slices.CompactFunc(ports, func(a, b execenv.ListeningPort) bool { return a.Port == b.Port })
}
