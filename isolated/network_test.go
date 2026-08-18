package isolated

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestParseAllowAcceptsIPv4Only(t *testing.T) {
	t.Parallel()
	got, err := parseAllow([]string{"10.0.0.1", "192.168.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !containsIP(got, net.ParseIP("10.0.0.1")) || !containsIP(got, net.ParseIP("192.168.1.9")) {
		t.Fatalf("allow = %v", got)
	}
	if containsIP(got, net.ParseIP("11.0.0.1")) {
		t.Fatal("11.0.0.1 should be denied")
	}
	if _, err := parseAllow([]string{"example.com"}); err == nil {
		t.Fatal("hostname dest accepted")
	}
	if _, err := parseAllow([]string{"::1"}); err == nil {
		t.Fatal("IPv6 dest accepted")
	}
}

func TestNewRejectsBadAllow(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		WorkDir: t.TempDir(),
		Slots:   1,
		Images:  []Image{writeCatalogImage(t, t.TempDir(), "default", "root")},
		Allow:   []string{"not-an-ip"},
	})
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("New() error = %v, want ErrInvalid", err)
	}
}

func TestEnsureAllowlistWithoutHostAllow(t *testing.T) {
	t.Parallel()
	host := testHost(t, func() error { return nil }, &recordingLauncher{})
	_, err := host.Ensure(t.Context(), execenv.Spec{
		ID:      "grant-1",
		Image:   "default",
		Network: execenv.NetworkAllowlist,
	})
	if !errors.Is(err, execenv.ErrNetwork) {
		t.Fatalf("Ensure() error = %v, want ErrNetwork", err)
	}
}

func TestEnsureAllowlistUsesHostDests(t *testing.T) {
	t.Parallel()
	launch := &recordingLauncher{}
	dir := t.TempDir()
	h, err := New(Config{
		WorkDir: t.TempDir(),
		Slots:   2,
		Images:  []Image{writeCatalogImage(t, dir, "default", "root")},
		Allow:   []string{"203.0.113.10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.probe = func() error { return nil }
	h.launch = launch
	env, err := h.Ensure(t.Context(), execenv.Spec{
		ID:      "grant-1",
		Image:   "default",
		Network: execenv.NetworkAllowlist,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Revoke(t.Context()) })
	launch.mu.Lock()
	req := launch.last
	launch.mu.Unlock()
	if req.Network != execenv.NetworkAllowlist {
		t.Fatalf("Network = %v", req.Network)
	}
	if len(req.Allow) != 1 || req.Allow[0] != "203.0.113.10" {
		t.Fatalf("Allow = %v, want host dests only", req.Allow)
	}
}

func TestEnsureNetworkChangeConflicts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h, err := New(Config{
		WorkDir: t.TempDir(),
		Slots:   2,
		Images:  []Image{writeCatalogImage(t, dir, "default", "root")},
		Allow:   []string{"203.0.113.10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.probe = func() error { return nil }
	h.launch = &recordingLauncher{}
	if _, err := h.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.Ensure(t.Context(), execenv.Spec{
		ID:      "grant-1",
		Image:   "default",
		Network: execenv.NetworkAllowlist,
	})
	if !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("Ensure() error = %v, want ErrConflict", err)
	}
}

func TestMachineConfigOmitsNICWhenNone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, err := writeMachineConfig(startRequest{
		ID:      "g1",
		Kernel:  filepath.Join(dir, "k"),
		Rootfs:  filepath.Join(dir, "r"),
		TreeDir: filepath.Join(dir, "workspace"),
		Network: execenv.NetworkNone,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := readMachine(t, path)
	if _, ok := doc["network-interfaces"]; ok {
		t.Fatalf("none must not configure a NIC: %v", doc)
	}
	args, _ := doc["boot-source"].(map[string]any)["boot_args"].(string)
	if strings.Contains(args, " ip=") {
		t.Fatalf("none must not assign a guest IP: %s", args)
	}
}

func TestMachineConfigAddsNICWhenAllowlist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	att := planAttach("g1", nil)
	path, err := writeMachineConfig(startRequest{
		ID:      "g1",
		Kernel:  filepath.Join(dir, "k"),
		Rootfs:  filepath.Join(dir, "r"),
		TreeDir: filepath.Join(dir, "workspace"),
		Network: execenv.NetworkAllowlist,
	}, &att)
	if err != nil {
		t.Fatal(err)
	}
	doc := readMachine(t, path)
	ifaces, ok := doc["network-interfaces"].([]any)
	if !ok || len(ifaces) != 1 {
		t.Fatalf("allowlist NIC = %v", doc["network-interfaces"])
	}
	iface := ifaces[0].(map[string]any)
	if iface["host_dev_name"] != att.Dev || iface["guest_mac"] != att.MAC {
		t.Fatalf("iface = %v", iface)
	}
	args, _ := doc["boot-source"].(map[string]any)["boot_args"].(string)
	if !strings.Contains(args, att.GuestIP) || !strings.Contains(args, att.HostIP) {
		t.Fatalf("boot_args = %s", args)
	}
}

func readMachine(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
