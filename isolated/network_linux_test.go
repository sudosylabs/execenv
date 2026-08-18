//go:build linux

package isolated

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// TestDeniedDestinationUnreachableFromGuestSide applies the same host
// filter a grant uses and dials from the guest-side namespace. A host
// process remaining able to reach the dest is not enough.
func TestDeniedDestinationUnreachableFromGuestSide(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("filter proof needs root")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("ip not available")
	}
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skip("iptables not available")
	}

	ns := fmt.Sprintf("et%x", time.Now().UnixNano()&0xffffff)
	hostVeth := "ec" + ns[len(ns)-6:]
	guestVeth := "eg" + ns[len(ns)-6:]
	if err := runIP("netns", "add", ns); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = runIP("netns", "delete", ns) })

	if err := runIP("link", "add", hostVeth, "type", "veth", "peer", "name", guestVeth); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runIP("link", "delete", hostVeth) })
	if err := runIP("link", "set", guestVeth, "netns", ns); err != nil {
		t.Fatal(err)
	}
	if err := runIP("addr", "add", "10.230.7.1/24", "dev", hostVeth); err != nil {
		t.Fatal(err)
	}
	if err := runIP("addr", "add", "10.230.7.99/32", "dev", hostVeth); err != nil {
		t.Fatal(err)
	}
	if err := runIP("link", "set", hostVeth, "up"); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("ip", "netns", "exec", ns, "ip", "addr", "add", "10.230.7.2/24", "dev", guestVeth).Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("ip", "netns", "exec", ns, "ip", "link", "set", guestVeth, "up").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("ip", "netns", "exec", ns, "ip", "route", "add", "default", "via", "10.230.7.1").Run(); err != nil {
		t.Fatal(err)
	}

	allowLn, err := net.Listen("tcp", "10.230.7.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = allowLn.Close() })
	go func() {
		for {
			c, err := allowLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	denyLn, err := net.Listen("tcp", "10.230.7.99:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = denyLn.Close() })
	go func() {
		for {
			c, err := denyLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	allowPort := strconv.Itoa(allowLn.Addr().(*net.TCPAddr).Port)
	denyPort := strconv.Itoa(denyLn.Addr().(*net.TCPAddr).Port)

	// Host process can still reach the denied dest. The guest must not.
	if c, err := net.DialTimeout("tcp", "10.230.7.99:"+denyPort, time.Second); err != nil {
		t.Fatalf("host cannot reach denied dest (setup): %v", err)
	} else {
		_ = c.Close()
	}

	chain := "ET" + ns[len(ns)-6:]
	allow, err := parseAllow([]string{"10.230.7.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := installFilter(hostVeth, chain, allow); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeFilter(hostVeth, chain) })

	if err := guestDial(ns, "10.230.7.1", allowPort); err != nil {
		t.Fatalf("guest cannot reach allowed dest: %v", err)
	}
	if err := guestDial(ns, "10.230.7.99", denyPort); err == nil {
		t.Fatal("guest reached a dest that is not on the host allowlist")
	}
}

func guestDial(ns, host, port string) error {
	cmd := exec.Command("ip", "netns", "exec", ns, "nc", "-z", "-w", "1", host, port)
	if _, err := exec.LookPath("nc"); err != nil {
		cmd = exec.Command("ip", "netns", "exec", ns, "timeout", "1", "bash", "-c",
			"echo >/dev/tcp/"+host+"/"+port)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func TestLiveAllowlistDeniesFromGuest(t *testing.T) {
	if os.Getenv("EXECENV_ISOLATION") != "1" {
		t.Skip("set EXECENV_ISOLATION=1 on a machine with isolation hardware")
	}
	kernel := os.Getenv("EXECENV_FIXTURE_KERNEL")
	rootfs := os.Getenv("EXECENV_FIXTURE_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set EXECENV_FIXTURE_KERNEL and EXECENV_FIXTURE_ROOTFS")
	}
	hash, err := Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	host, err := New(Config{
		WorkDir: t.TempDir(),
		Slots:   1,
		Allow:   []string{"203.0.113.1"},
		Images: []Image{{
			ID:     "fixture",
			Kernel: kernel,
			Rootfs: rootfs,
			Hash:   hash,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{
		ID:      "net-1",
		Image:   "fixture",
		Network: execenv.NetworkAllowlist,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Revoke(t.Context()) })
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	// 1.1.1.1 is not on the host allowlist. Success from this shell
	// would mean the guest has a general NIC.
	// A missing wget or no default route is not proof. Require wget,
	// then treat a completed TCP handshake as a leak.
	if _, err := term.Write([]byte("command -v wget >/dev/null || { echo NO_WGET; exit 0; }; wget -q -T 2 -O /dev/null http://1.1.1.1/ && echo LEAKED || echo DENIED\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	var buf []byte
	tmp := make([]byte, 256)
	for time.Now().Before(deadline) {
		n, _ := term.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if strings.Contains(string(buf), "NO_WGET") {
				t.Skip("fixture image has no wget")
			}
			if strings.Contains(string(buf), "LEAKED") {
				t.Fatal("guest reached a dest that is not on the host allowlist")
			}
			if strings.Contains(string(buf), "DENIED") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(buf), "DENIED") {
		t.Fatal("guest did not report that the dest was denied")
	}
}
