package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
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

func TestLoadIsolatedRequiresWorkDir(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "isolated",
		"images": [{"id": "default", "kernel": "vmlinux", "path": "rootfs.ext4"}],
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

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
