//go:build linux

package agent

import (
	"context"
	"net"
	"net/netip"
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/sudosylabs/execenv"
)

func TestListeningPortDiscovery(t *testing.T) {
	loop := netip.MustParseAddr("127.0.0.1")
	l4, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(loop, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer l4.Close()
	port := l4.Addr().(*net.TCPAddr).AddrPort().Port()
	l6, err := net.ListenTCP("tcp6", &net.TCPAddr{IP: net.IPv6loopback, Port: int(port)})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer l6.Close()
	tagOf := func(pid int) string {
		if pid == os.Getpid() {
			return "listener"
		}
		return ""
	}
	for _, tc := range []struct {
		name string
		dump func(context.Context, uint8) ([]listeningSocket, error)
	}{
		{"sock_diag", sockDiag},
		{"proc", func(_ context.Context, family uint8) ([]listeningSocket, error) { return procListening(family) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, listener := range []*net.TCPListener{l4, l6} {
				want := listener.Addr().(*net.TCPAddr).AddrPort()
				family := uint8(unix.AF_INET6)
				if want.Addr().Is4() || want.Addr().Is4In6() {
					family = unix.AF_INET
					want = netip.AddrPortFrom(want.Addr().Unmap(), want.Port())
				}
				got, err := tc.dump(t.Context(), family)
				if err != nil {
					t.Fatal(err)
				}
				i := slices.IndexFunc(got, func(s listeningSocket) bool { return s.addr == want })
				if i < 0 || socketTags(t.Context(), got[i:i+1], tagOf)[got[i].inode] != "listener" {
					t.Fatalf("missing socket or owner for %v", want)
				}
			}
		})
	}
	ports, err := listeningPorts(t.Context(), tagOf)
	if err != nil {
		t.Fatal(err)
	}
	var matching []execenv.ListeningPort
	for _, p := range ports {
		if p.Port == int(port) {
			matching = append(matching, p)
		}
	}
	want := []execenv.ListeningPort{{Port: int(port), Address: loop.String(), Tag: "listener"}}
	if !slices.Equal(matching, want) {
		t.Fatalf("socket list = %v, want %v", matching, want)
	}
}
