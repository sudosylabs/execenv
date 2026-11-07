//go:build linux

package isolated

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/sudosylabs/execenv"
)

// setupAllowlist creates a host tap and a per-grant filter so only the
// operator dests are reachable from the guest. Fail closed if the
// filter cannot be installed; never boot an open NIC.
func setupAllowlist(id execenv.ID, dests []string) (*netAttach, error) {
	allow, err := parseAllow(dests)
	if err != nil {
		return nil, err
	}
	if len(allow) == 0 {
		return nil, execenv.ErrNetwork
	}
	att := planAttach(id, allow)
	if err := runIP("tuntap", "add", "dev", att.Dev, "mode", "tap"); err != nil {
		return nil, err
	}
	cleanup := []func(){func() { _ = runIP("link", "delete", att.Dev) }}
	fail := func(err error) (*netAttach, error) {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
		return nil, err
	}
	if err := runIP("addr", "add", fmt.Sprintf("%s/%d", att.HostIP, att.Prefix), "dev", att.Dev); err != nil {
		return fail(err)
	}
	if err := runIP("link", "set", "dev", att.Dev, "up"); err != nil {
		return fail(err)
	}
	return guardAttach(att, allow, cleanup)
}

// setupSandboxAllowlist gives a sandbox grant a network namespace whose
// only interface is the inner end of a veth pair. The host end gets the
// same filter and NAT as a microVM tap. The sandbox runtime takes the
// namespace's interface and routes at start.
func setupSandboxAllowlist(id execenv.ID, dests []string) (*netAttach, error) {
	allow, err := parseAllow(dests)
	if err != nil {
		return nil, err
	}
	if len(allow) == 0 {
		return nil, execenv.ErrNetwork
	}
	att := planAttach(id, allow)
	ns := sandboxNetNSPrefix + att.Dev
	peer := sandboxPeer(att.Dev)
	if err := runIP("netns", "add", ns); err != nil {
		return nil, err
	}
	cleanup := []func(){func() { _ = runIP("netns", "delete", ns) }}
	fail := func(err error) (*netAttach, error) {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
		return nil, err
	}
	if err := runIP("link", "add", att.Dev, "type", "veth", "peer", "name", peer); err != nil {
		return fail(err)
	}
	cleanup = append(cleanup, func() { _ = runIP("link", "delete", att.Dev) })
	steps := [][]string{
		{"link", "set", peer, "netns", ns},
		{"addr", "add", fmt.Sprintf("%s/%d", att.HostIP, att.Prefix), "dev", att.Dev},
		{"link", "set", "dev", att.Dev, "up"},
		{"-n", ns, "addr", "add", fmt.Sprintf("%s/%d", att.GuestIP, att.Prefix), "dev", peer},
		{"-n", ns, "link", "set", "dev", peer, "up"},
		{"-n", ns, "link", "set", "dev", "lo", "up"},
		{"-n", ns, "route", "add", "default", "via", att.HostIP},
	}
	for _, step := range steps {
		if err := runIP(step...); err != nil {
			return fail(err)
		}
	}
	att.NetNS = "/var/run/netns/" + ns
	return guardAttach(att, allow, cleanup)
}

// guardAttach installs the per-grant filter and NAT on the host end of a
// grant's link. Failure unwinds everything, including cleanup's steps.
func guardAttach(att netAttach, allow []net.IPNet, cleanup []func()) (*netAttach, error) {
	fail := func(err error) (*netAttach, error) {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
		return nil, err
	}
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+att.Dev+"/disable_ipv6", []byte("1"), 0o644)
	chain := filterChain(att.Dev)
	if err := installFilter(att.Dev, chain, allow); err != nil {
		return fail(err)
	}
	cleanup = append(cleanup, func() { _ = removeFilter(att.Dev, chain) })
	guestNet := fmt.Sprintf("%s/%d", att.HostIP, att.Prefix)
	enableForward()
	if err := installNAT(guestNet, allow); err != nil {
		return fail(err)
	}
	cleanup = append(cleanup, func() { _ = removeNAT(guestNet, allow) })
	att.cleanup = func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}
	return &att, nil
}

// cleanSandboxNetworks removes grant network namespaces (and with them
// the namespace end of each veth pair) and the host ends left by a crashed
// daemon. Filters and NAT are swept by cleanStaleLinks.
func cleanSandboxNetworks() {
	entries, err := os.ReadDir("/var/run/netns")
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, sandboxNetNSPrefix) {
			continue
		}
		if dev := strings.TrimPrefix(name, sandboxNetNSPrefix); staleGrantDev(dev) {
			_ = runIP("link", "delete", dev)
		}
		_ = runIP("netns", "delete", name)
	}
}

// cleanStaleLinks removes what a crashed daemon's allowlist grants left on
// the host: grant taps, per-grant filter chains with their FORWARD and
// INPUT jumps, and grant MASQUERADE rules. NAT rules are found by listing,
// not rebuilt from configuration, so they are removed even if the operator
// allowlist changed across the restart. Only names and rules in exactly the
// shape this package creates are touched. Failures are ignored: a missing
// tool or rule leaves nothing to clean.
func cleanStaleLinks() {
	devs := map[string]bool{}
	if entries, err := os.ReadDir("/sys/class/net"); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			// tun_flags marks a tun/tap device; a sandbox's host veth end is
			// removed with its namespace instead.
			if _, err := os.Stat("/sys/class/net/" + name + "/tun_flags"); err == nil && staleGrantDev(name) {
				devs[name] = true
			}
		}
	}
	if out, err := exec.Command("iptables", "-S").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if dev, ok := staleGrantChain(line); ok {
				_ = removeFilter(dev, filterChain(dev))
			}
		}
	}
	if out, err := exec.Command("iptables", "-t", "nat", "-S", "POSTROUTING").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if args, ok := staleGrantNAT(line); ok {
				_ = runIPTables(args...)
			}
		}
	}
	for dev := range devs {
		_ = removeFilter(dev, filterChain(dev))
		_ = runIP("link", "delete", dev)
	}
}

func installFilter(iface, chain string, allow []net.IPNet) error {
	if err := runIPTables("-N", chain); err != nil {
		return err
	}
	for _, n := range allow {
		if err := runIPTables("-A", chain, "-d", n.String(), "-j", "ACCEPT"); err != nil {
			_ = removeFilter(iface, chain)
			return err
		}
	}
	if err := runIPTables("-A", chain, "-j", "DROP"); err != nil {
		_ = removeFilter(iface, chain)
		return err
	}
	if err := runIPTables("-I", "FORWARD", "1", "-i", iface, "-j", chain); err != nil {
		_ = removeFilter(iface, chain)
		return err
	}
	if err := runIPTables("-I", "FORWARD", "1", "-o", iface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		_ = removeFilter(iface, chain)
		return err
	}
	if err := runIPTables("-I", "INPUT", "1", "-i", iface, "-j", chain); err != nil {
		_ = removeFilter(iface, chain)
		return err
	}
	return nil
}

func installNAT(guestNet string, allow []net.IPNet) error {
	for _, n := range allow {
		if err := runIPTables("-t", "nat", "-A", "POSTROUTING", "-s", guestNet, "-d", n.String(), "-j", "MASQUERADE"); err != nil {
			_ = removeNAT(guestNet, allow)
			return err
		}
	}
	return nil
}

func removeNAT(guestNet string, allow []net.IPNet) error {
	for _, n := range allow {
		_ = runIPTables("-t", "nat", "-D", "POSTROUTING", "-s", guestNet, "-d", n.String(), "-j", "MASQUERADE")
	}
	return nil
}

func enableForward() {
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
}

func removeFilter(iface, chain string) error {
	_ = runIPTables("-D", "FORWARD", "-i", iface, "-j", chain)
	_ = runIPTables("-D", "FORWARD", "-o", iface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
	_ = runIPTables("-D", "INPUT", "-i", iface, "-j", chain)
	_ = runIPTables("-F", chain)
	_ = runIPTables("-X", chain)
	return nil
}

func runIP(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

func runIPTables(args ...string) error {
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}
