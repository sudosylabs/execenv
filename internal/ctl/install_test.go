package ctl

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/daemon"
	"github.com/sudosylabs/execenv/isolated"
)

func TestInstallRequiresBootstrap(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	err := Install(opts, "fixture", io.Discard)
	if err == nil {
		t.Fatal("Install() without bootstrap = nil")
	}
	if !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallUnknownID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	srv := newReleaseServer(t, map[string][]byte{
		"index.json": []byte(`{"kernel":"vmlinux","images":[{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`),
	})
	opts.ReleaseURL = srv.URL
	err := Install(opts, "python", io.Discard)
	if err == nil {
		t.Fatal("Install() unknown id = nil")
	}
	if !errors.Is(err, execenv.ErrUnknownImage) {
		t.Fatalf("error = %v, want ErrUnknownImage", err)
	}
}

func TestInstallFetchesFixtureAndLeavesToken(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	kernel := []byte("kernel-bytes")
	rootfs := []byte("rootfs-bytes")
	sum := writeDigest(t, root, kernel, rootfs)
	var kernelHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "index.json":
			_, _ = w.Write([]byte(`{"kernel":"vmlinux","images":[{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"` + sum + `"}]}`))
		case "vmlinux":
			kernelHits++
			_, _ = w.Write(kernel)
		case "rootfs-fixture.ext4":
			_, _ = w.Write(rootfs)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	opts.ReleaseURL = srv.URL

	reloads := 0
	opts.Reload = func() error { reloads++; return nil }

	var out bytes.Buffer
	if err := Install(opts, "fixture", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), sum) || strings.Contains(out.String(), before.Token) {
		t.Fatalf("stdout leaked secret: %q", out.String())
	}
	if !strings.Contains(out.String(), "installed=fixture") {
		t.Fatalf("stdout = %q", out.String())
	}
	cfg, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != before.Token {
		t.Fatal("install changed the token")
	}
	if len(cfg.Images) != 1 || cfg.Images[0].ID != "fixture" || cfg.Images[0].Hash != sum {
		t.Fatalf("images = %+v", cfg.Images)
	}
	got, err := isolated.Digest(cfg.Images[0].Kernel, cfg.Images[0].Rootfs)
	if err != nil || got != sum {
		t.Fatalf("digest = %q %v", got, err)
	}
	if reloads != 1 {
		t.Fatalf("reload = %d", reloads)
	}

	other := []byte("other-rootfs")
	otherDir := filepath.Join(root, "other")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherSum := writeDigest(t, otherDir, kernel, other)
	// Swap the handler so a second id is published against the same kernel.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "index.json":
			_, _ = w.Write([]byte(`{"kernel":"vmlinux","images":[` +
				`{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"` + sum + `"},` +
				`{"id":"other","rootfs":"rootfs-other.ext4","hash":"` + otherSum + `"}]}`))
		case "vmlinux":
			kernelHits++
			_, _ = w.Write(kernel)
		case "rootfs-fixture.ext4":
			_, _ = w.Write(rootfs)
		case "rootfs-other.ext4":
			_, _ = w.Write(other)
		default:
			http.NotFound(w, r)
		}
	})
	if err := Install(opts, "other", io.Discard); err != nil {
		t.Fatal(err)
	}
	if kernelHits != 1 {
		t.Fatalf("kernel downloads = %d, want 1", kernelHits)
	}

	host, err := isolated.New(isolated.Config{
		WorkDir: filepath.Join(root, "ready"),
		Images: []isolated.Image{{
			ID:     "fixture",
			Kernel: cfg.Images[0].Kernel,
			Rootfs: cfg.Images[0].Rootfs,
			Hash:   cfg.Images[0].Hash,
		}},
		Slots: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Images) != 1 || report.Images[0] != "fixture" {
		t.Fatalf("Ready() Images = %v", report.Images)
	}
}

func TestInstallUsesReleaseURLEnv(t *testing.T) {
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	kernel := []byte("k")
	rootfs := []byte("r")
	sum := writeDigest(t, root, kernel, rootfs)
	srv := newReleaseServer(t, map[string][]byte{
		"index.json":          []byte(`{"kernel":"vmlinux","images":[{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"` + sum + `"}]}`),
		"vmlinux":             kernel,
		"rootfs-fixture.ext4": rootfs,
	})
	t.Setenv(releaseURLEnv, srv.URL)
	opts.ReleaseURL = ""
	if err := Install(opts, "fixture", io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestInstallLeavesAllowAndResources(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.Sysconf, "host.json")
	cfg, err := daemon.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Network = "allowlist"
	cfg.Allow = []string{"198.51.100.2"}
	cfg.CPUMillis = 1500
	if err := daemon.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	kernel := []byte("k")
	rootfs := []byte("r")
	sum := writeDigest(t, root, kernel, rootfs)
	srv := newReleaseServer(t, map[string][]byte{
		"index.json":          []byte(`{"kernel":"vmlinux","images":[{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"` + sum + `"}]}`),
		"vmlinux":             kernel,
		"rootfs-fixture.ext4": rootfs,
	})
	opts.ReleaseURL = srv.URL
	opts.Reload = func() error { return nil }
	if err := Install(opts, "fixture", io.Discard); err != nil {
		t.Fatal(err)
	}
	again, err := daemon.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != cfg.Token || again.Network != "allowlist" || again.CPUMillis != 1500 {
		t.Fatalf("install mutated host configuration: %+v", again)
	}
	if len(again.Allow) != 1 || again.Allow[0] != "198.51.100.2" {
		t.Fatalf("allow = %v", again.Allow)
	}
}

func TestInstallBadHashLeavesExistingDisk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	kernel := []byte("k")
	good := []byte("good-rootfs")
	sum := writeDigest(t, root, kernel, good)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "index.json":
			_, _ = w.Write([]byte(`{"kernel":"vmlinux","images":[{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"` + sum + `"}]}`))
		case "vmlinux":
			_, _ = w.Write(kernel)
		case "rootfs-fixture.ext4":
			_, _ = w.Write(good)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	opts.ReleaseURL = srv.URL
	opts.Reload = func() error { return nil }
	if err := Install(opts, "fixture", io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	badIndex := `{"kernel":"vmlinux","images":[{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}]}`
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "index.json":
			_, _ = w.Write([]byte(badIndex))
		case "vmlinux":
			_, _ = w.Write(kernel)
		case "rootfs-fixture.ext4":
			_, _ = w.Write([]byte("corrupt"))
		default:
			http.NotFound(w, r)
		}
	})
	if err := Install(opts, "fixture", io.Discard); err == nil {
		t.Fatal("Install() accepted a bad hash")
	}
	body, err := os.ReadFile(cfg.Images[0].Rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(good) {
		t.Fatalf("good disk was overwritten: %q", body)
	}
}

func TestInstallRejectsHashMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	srv := newReleaseServer(t, map[string][]byte{
		"index.json":         []byte(`{"kernel":"vmlinux","images":[{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`),
		"vmlinux":            []byte("k"),
		"rootfs-fixture.ext4": []byte("r"),
	})
	opts.ReleaseURL = srv.URL
	err := Install(opts, "fixture", io.Discard)
	if err == nil {
		t.Fatal("Install() accepted a bad hash")
	}
	if !strings.Contains(err.Error(), "hash") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepoCatalogHasFixtureRecipe(t *testing.T) {
	t.Parallel()
	idx, err := loadIndexFile(filepath.Join("..", "..", "catalog", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if idx.Kernel != "vmlinux" {
		t.Fatalf("kernel = %q", idx.Kernel)
	}
	found := false
	for _, img := range idx.Images {
		if img.ID == "fixture" && img.Rootfs == "rootfs-fixture.ext4" {
			found = true
		}
	}
	if !found {
		t.Fatalf("index missing fixture: %+v", idx.Images)
	}
	recipe := filepath.Join("..", "..", "catalog", "fixture", "recipe.json")
	if _, err := os.Stat(recipe); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "catalog", "fixture", "Dockerfile")); err != nil {
		t.Fatal(err)
	}
}

func newReleaseServer(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeDigest(t *testing.T, root string, kernel, rootfs []byte) string {
	t.Helper()
	k := filepath.Join(root, "k")
	r := filepath.Join(root, "r")
	if err := os.WriteFile(k, kernel, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r, rootfs, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := isolated.Digest(k, r)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}
