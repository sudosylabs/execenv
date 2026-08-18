package isolated

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestNewRejectsIncompleteCatalog(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		WorkDir: t.TempDir(),
		Slots:   1,
		Images:  []Image{{ID: "default"}},
	})
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("New() error = %v, want ErrInvalid", err)
	}
}

func TestReadyListsOnlyVerifiedImages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := writeCatalogImage(t, dir, "default", "root-default")
	badPath := Image{
		ID:     "missing",
		Kernel: filepath.Join(dir, "no-kernel"),
		Rootfs: filepath.Join(dir, "no-rootfs"),
		Hash:   good.Hash,
	}
	corrupt := writeCatalogImage(t, dir, "corrupt", "root-corrupt")
	corrupt.Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	host := testHostWithImages(t, func() error { return nil }, &recordingLauncher{}, good, badPath, corrupt)

	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Images) != 1 || report.Images[0] != "default" {
		t.Fatalf("Ready() Images = %v, want [default]", report.Images)
	}
}

func TestEnsureRejectsUnknownCatalogId(t *testing.T) {
	t.Parallel()
	host := testHost(t, func() error { return nil }, &recordingLauncher{})
	_, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "nope"})
	if !errors.Is(err, execenv.ErrUnknownImage) {
		t.Fatalf("Ensure() error = %v, want ErrUnknownImage", err)
	}
}

func TestEnsureRejectsAbsentImage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := writeCatalogImage(t, dir, "default", "root-default")
	absent := Image{
		ID:     "gone",
		Kernel: good.Kernel,
		Rootfs: filepath.Join(dir, "gone.img"),
		Hash:   good.Hash,
	}
	host := testHostWithImages(t, func() error { return nil }, &recordingLauncher{}, good, absent)
	_, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "gone"})
	if !errors.Is(err, execenv.ErrUnknownImage) {
		t.Fatalf("Ensure() error = %v, want ErrUnknownImage", err)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDoesNotFetchMissingRootfs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := writeCatalogImage(t, dir, "default", "root")
	missing := filepath.Join(dir, "not-pulled.img")
	host := testHostWithImages(t, func() error { return nil }, &recordingLauncher{}, Image{
		ID:     "remote",
		Kernel: good.Kernel,
		Rootfs: missing,
		Hash:   good.Hash,
	})
	_, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "remote"})
	if !errors.Is(err, execenv.ErrUnknownImage) {
		t.Fatalf("Ensure() error = %v, want ErrUnknownImage", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Ensure created a missing rootfs")
	}
}

func TestReplacedKernelDropsTheId(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	img := writeCatalogImage(t, dir, "default", "root")
	host := testHostWithImages(t, func() error { return nil }, &recordingLauncher{}, img)
	if err := os.WriteFile(img.Kernel, []byte("replaced-kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Images) != 0 {
		t.Fatalf("Ready() Images = %v, want none after kernel change", report.Images)
	}
}

func writeCatalogImage(t *testing.T, dir, id, rootfsBody string) Image {
	t.Helper()
	kernel := filepath.Join(dir, id+".kernel")
	rootfs := filepath.Join(dir, id+".rootfs")
	if err := os.WriteFile(kernel, []byte("kernel-"+id), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte(rootfsBody), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	return Image{
		ID:     execenv.Image(id),
		Kernel: kernel,
		Rootfs: rootfs,
		Hash:   sum,
	}
}
