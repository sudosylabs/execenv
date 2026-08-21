package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/isolated"
)

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("Load() missing file = nil")
	}
}

func TestLoadRejectsEmptyRequiredFields(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{"listen":"127.0.0.1:0"}`))
	if err == nil {
		t.Fatal("Load() incomplete config = nil")
	}
}

func TestLoadRejectsProductionMemory(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:8443",
		"token": "secret",
		"security": "tls",
		"adapter": "memory",
		"tls_cert": "cert.pem",
		"tls_key": "key.pem",
		"images": [{"id": "default"}],
		"slots": 2
	}`))
	if err == nil {
		t.Fatal("Load() production memory = nil")
	}
}

func TestLoadAcceptsLocalMemory(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [{"id": "default"}],
		"slots": 2,
		"network": "none",
		"grace": "30s"
	}`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Listen != "127.0.0.1:0" || cfg.Slots != 2 {
		t.Fatalf("Load() listen=%q slots=%d", cfg.Listen, cfg.Slots)
	}
}

func TestLoadAcceptsMutualTLSWithoutToken(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:8443",
		"security": "tls",
		"adapter": "isolated",
		"tls_cert": "cert.pem",
		"tls_key": "key.pem",
		"tls_client_ca": "client-ca.pem",
		"work_dir": "/tmp/execenv",
		"images": [],
		"slots": 2
	}`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsInsecureLocalWithoutToken(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [],
		"slots": 1
	}`))
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("Load() error = %v, want ErrInvalid", err)
	}
}

func TestLoadDoesNotPutTokenInError(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "super-secret-token",
		"security": "insecure_local",
		"adapter": "memory"
	}`))
	if err == nil {
		t.Fatal("Load() expected error")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatal("error leaked token")
	}
}

func TestLoadIsolatedRejectsMalformedHash(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"work_dir": "/tmp/execenv",
		"images": [{"id": "default", "kernel": "vmlinux", "path": "rootfs.ext4", "hash": "nope"}],
		"slots": 2
	}`))
	if err == nil {
		t.Fatal("Load() isolated with short hash = nil")
	}
}

func TestLoadIsolatedRequiresImageHash(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"work_dir": "/tmp/execenv",
		"images": [{"id": "default", "kernel": "vmlinux", "path": "rootfs.ext4"}],
		"slots": 2
	}`))
	if err == nil {
		t.Fatal("Load() isolated without hash = nil")
	}
}

func TestLoadIsolatedRequiresWorkDir(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"images": [{"id": "default", "kernel": "vmlinux", "path": "rootfs.ext4", "hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
		"slots": 2
	}`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_, err = newAdapter(cfg)
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("newAdapter() error = %v, want ErrInvalid", err)
	}
}

func TestLoadAcceptsIsolatedWithNoImages(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"work_dir": "/tmp/execenv",
		"images": [],
		"slots": 2
	}`))
	if err != nil {
		t.Fatalf("Load() empty catalog error = %v", err)
	}
	if len(cfg.Images) != 0 {
		t.Fatalf("Images = %v", cfg.Images)
	}
}

func TestLoadAllowlistRequiresDests(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"work_dir": "/tmp/execenv",
		"images": [{"id": "default", "kernel": "vmlinux", "rootfs": "disk.ext4", "hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
		"slots": 2,
		"network": "allowlist"
	}`))
	if err == nil {
		t.Fatal("Load() allowlist without dests = nil")
	}
}

func TestLoadAcceptsRootfsKey(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"work_dir": "/tmp/execenv",
		"images": [{"id": "default", "kernel": "vmlinux", "rootfs": "disk.ext4", "hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
		"slots": 2
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Images[0].Rootfs != "disk.ext4" {
		t.Fatalf("Rootfs = %q", cfg.Images[0].Rootfs)
	}
}

func TestNewAdapterReadyOmitsUnverifiedImage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "disk.ext4")
	if err := os.WriteFile(kernel, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("r"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := isolated.Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"work_dir": "`+dir+`",
		"images": [
			{"id": "default", "kernel": "`+kernel+`", "rootfs": "`+rootfs+`", "hash": "`+sum+`"},
			{"id": "missing", "kernel": "`+kernel+`", "rootfs": "`+filepath.Join(dir, "gone")+`", "hash": "`+sum+`"}
		],
		"slots": 2
	}`))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newAdapter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Images) != 1 || report.Images[0] != "default" {
		t.Fatalf("Ready() Images = %v, want [default]", report.Images)
	}
}

func TestSaveRoundTripAllowAndResources(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [{"id": "default"}],
		"slots": 2
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Network = "allowlist"
	cfg.Allow = []string{"203.0.113.8"}
	cfg.CPUMillis = 1000
	cfg.MemoryBytes = 1 << 30
	cfg.DiskBytes = 20 << 30
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != "secret" || again.CPUMillis != 1000 || again.DiskBytes != 20<<30 {
		t.Fatalf("round-trip = %+v", again)
	}
	if len(again.Allow) != 1 || again.Allow[0] != "203.0.113.8" || again.Network != "allowlist" {
		t.Fatalf("allow = %+v", again)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"path"`) {
		t.Fatalf("Save emitted path alias: %s", raw)
	}
}

func TestSaveRejectsInvalidLeavesFile(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "keep-me",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [{"id": "default"}],
		"slots": 2
	}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = Save(path, Config{Listen: "127.0.0.1:0", Token: "keep-me"})
	if err == nil {
		t.Fatal("Save() invalid = nil")
	}
	if strings.Contains(err.Error(), "keep-me") {
		t.Fatal("Save error leaked token")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("Save changed the file on invalid config")
	}
}

func TestSaveEmitsRootfsNotPath(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"work_dir": "/tmp/execenv",
		"images": [{"id": "default", "kernel": "vmlinux", "path": "disk.ext4", "hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
		"slots": 2
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"rootfs": "disk.ext4"`) || strings.Contains(text, `"path"`) {
		t.Fatalf("Save = %s", text)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
