package isolated

import (
	"fmt"
	"net"

	"github.com/sudosylabs/execenv"
)

// parseAllow turns operator dests into IPv4 nets. Hostnames are rejected
// so the filter never depends on guest or host DNS.
func parseAllow(list []string) ([]net.IPNet, error) {
	out := make([]net.IPNet, 0, len(list))
	for _, raw := range list {
		n, err := parseDest(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func parseDest(raw string) (net.IPNet, error) {
	if raw == "" {
		return net.IPNet{}, execenv.ErrInvalid
	}
	if _, n, err := net.ParseCIDR(raw); err == nil {
		if n.IP.To4() == nil {
			return net.IPNet{}, execenv.ErrInvalid
		}
		return *n, nil
	}
	ip := net.ParseIP(raw)
	if ip == nil || ip.To4() == nil {
		return net.IPNet{}, execenv.ErrInvalid
	}
	return net.IPNet{IP: ip.To4(), Mask: net.CIDRMask(32, 32)}, nil
}

func containsIP(allow []net.IPNet, ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	for _, n := range allow {
		if n.Contains(v4) {
			return true
		}
	}
	return false
}

// netAttach is the host-side path for one allowlist grant. Exported
// isolated types do not mention the device names inside.
type netAttach struct {
	Dev     string
	MAC     string
	GuestIP string
	HostIP  string
	Prefix  int
	Allow   []net.IPNet
	cleanup func()
}

func (a *netAttach) close() {
	if a != nil && a.cleanup != nil {
		a.cleanup()
		a.cleanup = nil
	}
}

func planAttach(id execenv.ID, allow []net.IPNet) netAttach {
	sum := guestCID(id)
	octet := byte(sum%200 + 20)
	return netAttach{
		Dev:     fmt.Sprintf("e%08x", sum),
		MAC:     fmt.Sprintf("06:00:ac:10:%02x:02", octet),
		GuestIP: fmt.Sprintf("169.254.%d.2", octet),
		HostIP:  fmt.Sprintf("169.254.%d.1", octet),
		Prefix:  30,
		Allow:   allow,
	}
}
