package isolated

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

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

// preservesNetworkPolicy reports whether a live machine still matches
// the operator's policy. The order and spelling of equal CIDRs may change.
func preservesNetworkPolicy(mode execenv.Network, record *netRecord, configured []string) bool {
	if mode != execenv.NetworkAllowlist {
		return record == nil
	}
	if record == nil || len(configured) == 0 {
		return false
	}
	before, err := parseAllow(record.Allow)
	if err != nil {
		return false
	}
	after, err := parseAllow(configured)
	if err != nil {
		return false
	}
	counts := make(map[string]bool, len(before))
	for _, n := range before {
		counts[n.String()] = false
	}
	for _, n := range after {
		key := n.String()
		if _, ok := counts[key]; !ok {
			return false
		}
		counts[key] = true
	}
	for _, retained := range counts {
		if !retained {
			return false
		}
	}
	return true
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

// Stale network state left by a crashed daemon is recognised only by the
// exact names and rules planAttach and guardAttach produce, so cleanup never
// touches an interface, chain or NAT rule it did not create.
var (
	grantDevPattern   = regexp.MustCompile(`^e[0-9a-f]{8}$`)
	grantChainPattern = regexp.MustCompile(`^-N (E[0-9a-f]{8})$`)
	grantNATPattern   = regexp.MustCompile(`^-A POSTROUTING -s 169\.254\.(\d{1,3})\.0/30 -d \S+ -j MASQUERADE$`)
)

// filterChain names the per-grant filter chain for a grant device.
func filterChain(dev string) string {
	name := "E" + strings.TrimPrefix(dev, "e")
	if len(name) > 28 {
		return name[:28]
	}
	return name
}

// staleGrantDev reports whether an interface name is one planAttach makes.
func staleGrantDev(name string) bool { return grantDevPattern.MatchString(name) }

// staleGrantChain extracts the grant device of a filter chain declared in
// `iptables -S` output.
func staleGrantChain(line string) (string, bool) {
	m := grantChainPattern.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", false
	}
	return "e" + m[1][1:], true
}

// staleGrantNAT turns one `iptables -t nat -S POSTROUTING` line for a grant
// link into the arguments that delete it. Grant links use 169.254.N.0/30
// with N in planAttach's 20..219 range.
func staleGrantNAT(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	m := grantNATPattern.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	octet, err := strconv.Atoi(m[1])
	if err != nil || octet < 20 || octet > 219 {
		return nil, false
	}
	args := strings.Fields(line)
	args[0] = "-D"
	return append([]string{"-t", "nat"}, args...), true
}
