package execenv_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/isolated"
)

func TestCLIHasNoBakeCommand(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("cmd", "execenv", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"bake"`) || strings.Contains(string(body), "runBake") {
		t.Fatal("execenv CLI still has a bake command")
	}
}

func TestBakeScriptIsCIOnlyAndFailsClosed(t *testing.T) {
	t.Parallel()
	script := filepath.Join("scripts", "bake")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("scripts/bake is not executable")
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "mcr.microsoft.com/devcontainers/universal:linux") {
		t.Fatal("scripts/bake does not pin the universal-class source")
	}
	if strings.Contains(text, "execenv bake") {
		t.Fatal("scripts/bake still invokes an execenv bake command")
	}
	dir := t.TempDir()
	kernel := filepath.Join(dir, "k")
	agent := filepath.Join(dir, "a")
	if err := os.WriteFile(kernel, []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, "--out", filepath.Join(dir, "out"), "--kernel", kernel, "--agent", agent)
	cmd.Env = []string{"PATH=/nonexistent"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("bake succeeded without docker")
	}
	if !strings.Contains(string(out), "docker or podman") {
		t.Fatalf("error = %s", out)
	}
}

func TestGuestInitStartsAgentWithoutNetwork(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("scripts", "guest.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, execenv.GuestHome) || !strings.Contains(text, "agent") {
		t.Fatal("guest init does not start the agent at home")
	}
	if !strings.Contains(text, execenv.GuestBin) {
		t.Fatal("guest init does not exec the installed binary")
	}
	if strings.Contains(text, "apk ") || strings.Contains(text, "apt") || strings.Contains(text, "curl ") {
		t.Fatal("guest init reaches the network")
	}
}

func TestCatalogHashIsKernelThenRootfs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := isolated.Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "cat -- \"$1\" \"$2\" | sha256sum | awk '{print $1}'", "hash", kernel, rootfs)
	out, err := cmd.Output()
	if err != nil {
		t.Skip("sha256sum not available")
	}
	if strings.TrimSpace(string(out)) != sum {
		t.Fatalf("script hash = %q, isolated.Digest = %q", strings.TrimSpace(string(out)), sum)
	}
}

func TestDaemonDoesNotImportBake(t *testing.T) {
	t.Parallel()
	err := filepath.Walk("daemon", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		if strings.Contains(text, "/bake") || strings.Contains(text, "docker") || strings.Contains(text, "podman") {
			t.Fatalf("%s mentions bake or a container client", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
