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

func TestFirstLanguageRecipesExist(t *testing.T) {
	t.Parallel()
	wantFROM := map[string]string{
		"python": "FROM python:",
		"node":   "FROM node:",
		"go":     "FROM golang:",
		"java":   "FROM eclipse-temurin:",
	}
	for id, prefix := range wantFROM {
		recipe := filepath.Join("catalog", id, "recipe.json")
		if _, err := os.Stat(recipe); err != nil {
			t.Fatalf("%s: %v", recipe, err)
		}
		df := filepath.Join("catalog", id, "Dockerfile")
		body, err := os.ReadFile(df)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), prefix) {
			t.Fatalf("%s does not FROM official slim image: %s", df, body)
		}
		if strings.Contains(string(body), "execenv") {
			t.Fatalf("%s installs the agent; bake must inject it", df)
		}
	}
	if _, err := os.Stat(filepath.Join("catalog", "default", "recipe.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join("catalog", "default", "Dockerfile")); !os.IsNotExist(err) {
		t.Fatal("default must not have a Dockerfile; it uses the universal-class source")
	}
}

func TestAssembleReleaseWritesIndexAndChecksums(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs-python.ext4")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "execenv"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join("scripts", "assemble-release"), dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("assemble-release: %v\n%s", err, out)
	}
	idx, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := isolated.Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(idx), `"id": "python"`) || !strings.Contains(string(idx), sum) {
		t.Fatalf("index = %s", idx)
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sums), "execenv") || !strings.Contains(string(sums), "index.json") {
		t.Fatalf("SHA256SUMS = %s", sums)
	}
}

func TestPinImageAppendsWithoutRewritingOtherHashes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	python := filepath.Join(dir, "rootfs-python.ext4")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "execenv"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join("scripts", "assemble-release"), dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("assemble-release: %v\n%s", err, out)
	}
	pythonHash, err := isolated.Digest(kernel, python)
	if err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(dir, "rootfs-default.ext4")
	if err := os.WriteFile(def, []byte("default-disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin := exec.Command(filepath.Join("scripts", "pin-image"),
		"--dir", dir, "--id", "default", "--rootfs", def, "--kernel", kernel)
	if out, err := pin.CombinedOutput(); err != nil {
		t.Fatalf("pin-image: %v\n%s", err, out)
	}
	idx, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(idx)
	if !strings.Contains(text, pythonHash) || !strings.Contains(text, `"id": "default"`) {
		t.Fatalf("index = %s", text)
	}
	again := exec.Command(filepath.Join("scripts", "pin-image"),
		"--dir", dir, "--id", "default", "--rootfs", def, "--kernel", kernel)
	if out, err := again.CombinedOutput(); err != nil {
		t.Fatalf("pin-image again: %v\n%s", err, out)
	}
	other := filepath.Join(dir, "other.ext4")
	if err := os.WriteFile(other, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := exec.Command(filepath.Join("scripts", "pin-image"),
		"--dir", dir, "--id", "default", "--rootfs", other, "--kernel", kernel)
	if err := bad.Run(); err == nil {
		t.Fatal("pin-image accepted a hash change")
	}
}

func TestExecenvDoesNotImportManager(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("go", "list", "-f", "{{ join .Imports \"\\n\" }}", "./cmd/execenv")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "github.com/spf13/cobra") {
		t.Fatal("execenv imports cobra")
	}
	if strings.Contains(text, "github.com/sudosylabs/execenv/internal/ctl") {
		t.Fatal("execenv imports the manager package")
	}
}

func TestCLIHasOnlyDaemonAndAgent(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("cmd", "execenv", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, `"bake"`) || strings.Contains(text, "runBake") {
		t.Fatal("execenv CLI still has a bake command")
	}
	if strings.Contains(text, `"bootstrap"`) || strings.Contains(text, "runBootstrap") {
		t.Fatal("execenv CLI still has a bootstrap command")
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
	if !strings.Contains(text, "--platform linux/amd64") {
		t.Fatal("scripts/bake does not pin linux/amd64")
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

func TestShellBootstrapIsRetired(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(filepath.Join("scripts", "bootstrap")); !os.IsNotExist(err) {
		t.Fatal("scripts/bootstrap still exists; host install is execenvctl")
	}
	body, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "scripts/bootstrap") {
		t.Fatal("Makefile still invokes the shell installer")
	}
	if !strings.Contains(string(body), "execenvctl bootstrap") {
		t.Fatal("Makefile does not point operators at execenvctl")
	}
}
