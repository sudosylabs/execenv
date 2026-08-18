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

func filterChain(dev string) string {
	name := "E" + strings.TrimPrefix(dev, "e")
	if len(name) > 28 {
		return name[:28]
	}
	return name
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
