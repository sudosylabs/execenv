package isolated

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestMachineConfigHasWorkspaceDriveAndBalloon(t *testing.T) {
	t.Parallel()
	grant := filepath.Join(t.TempDir(), "grant")
	req := startRequest{
		ID:      "grant-1",
		Kernel:  "/k",
		Rootfs:  "/r",
		TreeDir: filepath.Join(grant, workspaceName),
		Disk:    filepath.Join(grant, diskName),
	}
	if err := os.MkdirAll(req.TreeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := writeMachineConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Drives []struct {
			ID       string `json:"drive_id"`
			Path     string `json:"path_on_host"`
			Root     bool   `json:"is_root_device"`
			ReadOnly bool   `json:"is_read_only"`
		} `json:"drives"`
		Balloon *struct {
			Amount    int  `json:"amount_mib"`
			Deflate   bool `json:"deflate_on_oom"`
			Reporting bool `json:"free_page_reporting"`
		} `json:"balloon"`
		Boot struct {
			Args string `json:"boot_args"`
		} `json:"boot-source"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Drives) != 2 {
		t.Fatalf("drives = %+v", doc.Drives)
	}
	// Order matters: the root device is first, so the workspace disk is the
	// guest's second block device, which the guest init mounts.
	if !doc.Drives[0].Root || !doc.Drives[0].ReadOnly || doc.Drives[0].Path != "/r" {
		t.Fatalf("root drive = %+v", doc.Drives[0])
	}
	if doc.Drives[1].Root || doc.Drives[1].ReadOnly || doc.Drives[1].Path != req.Disk {
		t.Fatalf("workspace drive = %+v", doc.Drives[1])
	}
	if doc.Balloon == nil || doc.Balloon.Amount != 0 || !doc.Balloon.Deflate || !doc.Balloon.Reporting {
		t.Fatalf("balloon = %+v", doc.Balloon)
	}
	if strings.Contains(doc.Boot.Args, "home=") {
		t.Fatalf("boot args still name a home: %q", doc.Boot.Args)
	}
}

func TestCreateWorkspaceDiskIsSparseAndFormatted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	mkfs := filepath.Join(dir, "mkfs")
	if err := os.WriteFile(mkfs, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(dir, "grant", diskName)
	if err := createWorkspaceDisk(t.Context(), mkfs, disk, minDiskBytes); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != minDiskBytes {
		t.Fatalf("disk size = %d", info.Size())
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-F", diskLabel, "lazy_itable_init=1,lazy_journal_init=1,nodiscard", disk} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("mkfs args %q missing %q", args, want)
		}
	}
	if err := createWorkspaceDisk(t.Context(), mkfs, disk, minDiskBytes); err == nil {
		t.Fatal("an existing disk was overwritten")
	}
	if err := createWorkspaceDisk(t.Context(), mkfs, filepath.Join(dir, "small"), minDiskBytes-1); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("undersized disk error = %v", err)
	}
}

func TestCreateWorkspaceDiskRemovesFileWhenFormatFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	disk := filepath.Join(dir, diskName)
	if err := createWorkspaceDisk(t.Context(), "/bin/false", disk, minDiskBytes); err == nil {
		t.Fatal("format failure succeeded")
	}
	if _, err := os.Stat(disk); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disk left behind: %v", err)
	}
}

func TestEnsurePassesWorkspaceDiskAndRevokeRemovesIt(t *testing.T) {
	t.Parallel()
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	launch.mu.Lock()
	req := launch.last
	launch.mu.Unlock()
	grantDir := filepath.Join(host.cfg.WorkDir, "grants", "grant-1")
	if req.Disk != filepath.Join(grantDir, diskName) || req.DiskBytes != defaultDiskBytes {
		t.Fatalf("disk = %q (%d)", req.Disk, req.DiskBytes)
	}
	if err := os.WriteFile(req.Disk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(req.Disk); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace disk survived revoke: %v", err)
	}
}

func TestNewValidatesDiskSize(t *testing.T) {
	t.Parallel()
	image := writeCatalogImage(t, t.TempDir(), "default", "root")
	for name, cfg := range map[string]Config{
		"undersized disk": {DiskBytes: minDiskBytes - 1},
	} {
		cfg.WorkDir = t.TempDir()
		cfg.Images = []Image{image}
		if _, err := New(cfg); !errors.Is(err, execenv.ErrInvalid) {
			t.Errorf("%s: New() error = %v, want ErrInvalid", name, err)
		}
	}
}
