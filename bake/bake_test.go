package bake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/isolated"
)

func TestDefaultSourceIsUniversalClass(t *testing.T) {
	t.Parallel()
	if DefaultID != "default" {
		t.Fatalf("DefaultID = %q", DefaultID)
	}
	if DefaultSource != "mcr.microsoft.com/devcontainers/universal:linux" {
		t.Fatalf("DefaultSource = %q", DefaultSource)
	}
}

func TestBakeWritesCatalogAndInstallsAgent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kernel := writeFile(t, dir, "in-kernel", "kernel-bytes")
	agentBin := writeFile(t, dir, "in-agent", "agent-bytes")
	out := filepath.Join(dir, "out")

	var exported, packed bool
	res, err := run(t.Context(), Request{
		Kernel: kernel,
		Agent:  agentBin,
		OutDir: out,
	}, steps{
		exportFS: func(_ context.Context, image, dockerfile, dest string) (string, error) {
			exported = true
			if image != DefaultSource || dockerfile != "" {
				t.Fatalf("export image=%q dockerfile=%q", image, dockerfile)
			}
			return DefaultSource, os.WriteFile(filepath.Join(dest, "etc-os"), []byte("alpine"), 0o644)
		},
		packFS: func(_ context.Context, staging, dest string, size int64) error {
			packed = true
			if size <= 0 {
				t.Fatal("pack size must be positive")
			}
			if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(guestRel(execenv.GuestBin)))); err != nil {
				t.Fatal("agent binary not installed into staging")
			}
			init := filepath.Join(staging, filepath.FromSlash(guestRel(execenv.GuestInit)))
			body, err := os.ReadFile(init)
			if err != nil {
				t.Fatal("guest init not installed")
			}
			text := string(body)
			if !strings.Contains(text, execenv.GuestHome) || !strings.Contains(text, "agent") {
				t.Fatal("guest init does not start the agent at home")
			}
			if strings.Contains(text, "apk ") || strings.Contains(text, "apt") || strings.Contains(text, "curl ") {
				t.Fatal("guest init reaches the network")
			}
			return os.WriteFile(dest, []byte("rootfs-bytes"), 0o644)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exported || !packed {
		t.Fatal("export or pack skipped")
	}
	if res.ID != DefaultID || res.Source != DefaultSource {
		t.Fatalf("result id=%q source=%q", res.ID, res.Source)
	}
	want, err := isolated.Digest(res.Kernel, res.Rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Hash != want || !isolated.ValidDigest(res.Hash) {
		t.Fatalf("hash = %q, want %q", res.Hash, want)
	}
	raw, err := os.ReadFile(filepath.Join(out, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["id"] != "default" || doc["hash"] != res.Hash || doc["kernel"] != DefaultKernelFile {
		t.Fatalf("catalog.json = %s", raw)
	}
}

func TestBakeDockerfileIsExtraCatalogId(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	df := writeFile(t, dir, "lang.Dockerfile", "FROM alpine\n")
	var gotDockerfile string
	_, err := run(t.Context(), Request{
		ID:         "python",
		Dockerfile: df,
		Kernel:     writeFile(t, dir, "k", "k"),
		Agent:      writeFile(t, dir, "a", "a"),
		OutDir:     filepath.Join(dir, "out"),
	}, steps{
		exportFS: func(_ context.Context, image, dockerfile, dest string) (string, error) {
			gotDockerfile = dockerfile
			if image != "" {
				t.Fatalf("dockerfile bake still passed source %q", image)
			}
			return dockerfile, os.MkdirAll(dest, 0o755)
		},
		packFS: func(_ context.Context, _, dest string, _ int64) error {
			return os.WriteFile(dest, []byte("fs"), 0o644)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotDockerfile != df {
		t.Fatalf("dockerfile = %q", gotDockerfile)
	}
}

func TestBakeRejectsSourceAndDockerfile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := run(t.Context(), Request{
		Source:     DefaultSource,
		Dockerfile: writeFile(t, dir, "D", "FROM scratch\n"),
		Kernel:     writeFile(t, dir, "k", "k"),
		Agent:      writeFile(t, dir, "a", "a"),
		OutDir:     filepath.Join(dir, "out"),
	}, steps{
		exportFS: func(context.Context, string, string, string) (string, error) { return "", nil },
		packFS:   func(context.Context, string, string, int64) error { return nil },
	})
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
}

func TestBakeRejectsMissingKernel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Run(t.Context(), Request{
		Agent:  writeFile(t, dir, "a", "a"),
		OutDir: filepath.Join(dir, "out"),
		Kernel: filepath.Join(dir, "missing"),
	})
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
}

func TestLinuxAgentRejectsPlainFile(t *testing.T) {
	t.Parallel()
	path := writeFile(t, t.TempDir(), "not-elf", "#!/bin/sh\n")
	if LinuxAgent(path) {
		t.Fatal("plain file reported as a linux guest binary")
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()
	got, err := ParseSize("20G")
	if err != nil || got != 20<<30 {
		t.Fatalf("ParseSize(20G) = %d, %v", got, err)
	}
	if _, err := ParseSize("nope"); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("ParseSize(nope) = %v", err)
	}
}

func TestDaemonAndIsolatedDoNotImportBake(t *testing.T) {
	t.Parallel()
	roots := []string{
		filepath.Join("..", "daemon"),
		filepath.Join("..", "isolated"),
	}
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(body)
			if strings.Contains(text, "github.com/sudosylabs/execenv/bake") {
				t.Fatalf("%s imports bake", path)
			}
			if strings.Contains(text, "docker") || strings.Contains(text, "podman") {
				t.Fatalf("%s mentions a container client", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportedNamesStayNeutral(t *testing.T) {
	t.Parallel()
	banned := []string{"Firecracker", "firecracker", "jailer", "Jailer", "KVM", "kvm", "vsock"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(ent.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, word := range banned {
			if strings.Contains(text, word) {
				t.Fatalf("%s contains %q", ent.Name(), word)
			}
		}
	}
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
