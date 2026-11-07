package isolated

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestStaleGrantDevMatchesOnlyPlannedNames(t *testing.T) {
	t.Parallel()
	att := planAttach("grant-1", nil)
	if !staleGrantDev(att.Dev) {
		t.Fatalf("planned device %q not recognised", att.Dev)
	}
	for _, name := range []string{"eth0", "eno1", "ens3", "enp0s31f6", "e1234567", "e1234567890", "eABCDEF12", "docker0", "p1234abcd"} {
		if staleGrantDev(name) {
			t.Errorf("%q matched as a grant device", name)
		}
	}
}

func TestStaleGrantChainMatchesFilterChain(t *testing.T) {
	t.Parallel()
	att := planAttach("grant-1", nil)
	dev, ok := staleGrantChain("-N " + filterChain(att.Dev) + "\n")
	if !ok || dev != att.Dev {
		t.Fatalf("chain -> %q, %v; want %q", dev, ok, att.Dev)
	}
	for _, line := range []string{"-N DOCKER", "-N E1234", "-A FORWARD -i " + att.Dev + " -j " + filterChain(att.Dev), "-P FORWARD ACCEPT"} {
		if _, ok := staleGrantChain(line); ok {
			t.Errorf("%q matched as a grant chain", line)
		}
	}
}

func TestStaleGrantNATDeletesOnlyGrantMasquerade(t *testing.T) {
	t.Parallel()
	att := planAttach("grant-1", nil)
	octet := strings.Split(att.HostIP, ".")[2]
	// iptables -S prints the source normalised to its network address.
	line := fmt.Sprintf("-A POSTROUTING -s 169.254.%s.0/30 -d 10.0.0.5/32 -j MASQUERADE", octet)
	args, ok := staleGrantNAT(line)
	want := []string{"-t", "nat", "-D", "POSTROUTING", "-s", "169.254." + octet + ".0/30", "-d", "10.0.0.5/32", "-j", "MASQUERADE"}
	if !ok || !slices.Equal(args, want) {
		t.Fatalf("args = %v, %v", args, ok)
	}
	for _, other := range []string{
		"-A POSTROUTING -s 172.17.0.0/16 ! -o docker0 -j MASQUERADE",
		"-A POSTROUTING -s 169.254.5.0/30 -d 10.0.0.5/32 -j MASQUERADE",
		"-A POSTROUTING -s 169.254.42.0/24 -d 10.0.0.5/32 -j MASQUERADE",
		"-A POSTROUTING -s 169.254.42.0/30 -d 10.0.0.5/32 -j SNAT --to-source 1.2.3.4",
		"-A POSTROUTING -s 169.254.42.0/30 -d 10.0.0.5/32 -m comment --comment \"x\" -j MASQUERADE",
		"-P POSTROUTING ACCEPT",
	} {
		if _, ok := staleGrantNAT(other); ok {
			t.Errorf("%q matched as a grant NAT rule", other)
		}
	}
}

func TestPlannedOctetsStayInsideStaleRange(t *testing.T) {
	t.Parallel()
	for i := range 2000 {
		att := planAttach(executionID(i), nil)
		octet := strings.Split(att.HostIP, ".")[2]
		line := "-A POSTROUTING -s 169.254." + octet + ".0/30 -d 10.0.0.5/32 -j MASQUERADE"
		if _, ok := staleGrantNAT(line); !ok || !staleGrantDev(att.Dev) {
			t.Fatalf("grant %d (%s, %s) would escape stale cleanup", i, att.Dev, att.HostIP)
		}
	}
}

func executionID(i int) execenv.ID { return execenv.ID(fmt.Sprintf("grant-%d", i)) }
