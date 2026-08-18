package execenv_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/daemon"
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

func writeArtifactDir(t *testing.T, root string) string {
	t.Helper()
	arts := filepath.Join(root, "arts")
	if err := os.MkdirAll(arts, 0o755); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(arts, "vmlinux")
	rootfs := filepath.Join(arts, "rootfs.ext4")
	if err := os.WriteFile(kernel, []byte("kernel-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("rootfs-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := isolated.Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	catalog := "{\n  \"id\": \"default\",\n  \"kernel\": \"vmlinux\",\n  \"rootfs\": \"rootfs.ext4\",\n  \"hash\": \"" + sum + "\"\n}\n"
	if err := os.WriteFile(filepath.Join(arts, "catalog.json"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	return arts
}

func TestBootstrapRequiresBothRuntimeAndSupervisor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	device := filepath.Join(root, "kvm")
	if err := os.WriteFile(device, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}
	arts := writeArtifactDir(t, root)
	bindir := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"execenv", "firecracker"} {
		if err := os.WriteFile(filepath.Join(bindir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join("scripts", "bootstrap"),
		"--device", device,
		"--prefix", filepath.Join(root, "usr", "local"),
		"--sysconf", filepath.Join(root, "etc", "execenv"),
		"--state", filepath.Join(root, "var", "lib", "execenv"),
		"--artifact-dir", arts,
		"--execenv", filepath.Join(bindir, "execenv"),
		"--no-start",
		"--no-fetch",
		"--insecure",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("bootstrap succeeded with only the runtime binary")
	}
	if !strings.Contains(string(out), "supervisor") {
		t.Fatalf("error = %s", out)
	}
}

func TestBootstrapFailsClosedWithoutDevice(t *testing.T) {
	t.Parallel()
	script := filepath.Join("scripts", "bootstrap")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("scripts/bootstrap is not executable")
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "docker pull") || strings.Contains(text, "podman pull") {
		t.Fatal("bootstrap pulls containers")
	}
	dir := t.TempDir()
	cmd := exec.Command(script,
		"--device", filepath.Join(dir, "no-kvm"),
		"--artifact-dir", dir,
		"--execenv", filepath.Join(dir, "execenv"),
		"--no-start",
		"--no-fetch",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("bootstrap succeeded without an isolation device")
	}
	msg := string(out)
	if !strings.Contains(msg, "isolation device") || !strings.Contains(msg, "no container fallback") {
		t.Fatalf("error = %s", msg)
	}
}

func TestBootstrapWritesConfigWithoutLeakingSecrets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	device := filepath.Join(root, "kvm")
	if err := os.WriteFile(device, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}
	arts := filepath.Join(root, "arts")
	if err := os.MkdirAll(arts, 0o755); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(arts, "vmlinux")
	rootfs := filepath.Join(arts, "rootfs.ext4")
	if err := os.WriteFile(kernel, []byte("kernel-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("rootfs-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := isolated.Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	catalog := "{\n  \"id\": \"default\",\n  \"kernel\": \"vmlinux\",\n  \"rootfs\": \"rootfs.ext4\",\n  \"hash\": \"" + sum + "\"\n}\n"
	if err := os.WriteFile(filepath.Join(arts, "catalog.json"), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	bindir := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"execenv", "firecracker", "jailer"} {
		path := filepath.Join(bindir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prefix := filepath.Join(root, "usr", "local")
	sysconf := filepath.Join(root, "etc", "execenv")
	state := filepath.Join(root, "var", "lib", "execenv")
	run := func() (string, error) {
		cmd := exec.Command(filepath.Join("scripts", "bootstrap"),
			"--device", device,
			"--prefix", prefix,
			"--sysconf", sysconf,
			"--state", state,
			"--artifact-dir", arts,
			"--execenv", filepath.Join(bindir, "execenv"),
			"--no-start",
			"--no-fetch",
			"--insecure",
			"--listen", "127.0.0.1:8443",
			"--slots", "8",
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	stdout, err := run()
	if err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, stdout)
	}
	if strings.Contains(stdout, sum) {
		t.Fatal("stdout printed the catalog hash")
	}
	cfgPath := filepath.Join(sysconf, "host.json")
	cfg, err := daemon.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token == "" || strings.Contains(stdout, cfg.Token) {
		t.Fatal("token missing or leaked on stdout")
	}
	if cfg.Adapter != "isolated" || cfg.Images[0].ID != "default" || cfg.Images[0].Hash != sum {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Images[0].Kernel == "" || cfg.Images[0].Rootfs == "" {
		t.Fatal("config missing kernel/rootfs paths")
	}
	firstToken := cfg.Token
	stdout, err = run()
	if err != nil {
		t.Fatalf("second bootstrap: %v\n%s", err, stdout)
	}
	again, err := daemon.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != firstToken {
		t.Fatal("re-run rotated the token")
	}
	unit := filepath.Join(root, "etc", "systemd", "system", "execenv.service")
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "-config "+cfgPath) {
		t.Fatalf("unit = %s", body)
	}
	if strings.Contains(string(body), firstToken) {
		t.Fatal("systemd unit contains the token")
	}
}
