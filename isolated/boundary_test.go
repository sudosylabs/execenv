package isolated

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
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

func TestNewValidatesIsolationClass(t *testing.T) {
	t.Parallel()
	image := writeCatalogImage(t, t.TempDir(), "default", "root")
	for name, cfg := range map[string]Config{
		"unknown class":            {Isolation: "container"},
		"user-kernel with journal": {Isolation: execenv.IsolationUserKernel, RequireJournal: true},
		"undersized disk":          {DiskBytes: minDiskBytes - 1},
	} {
		cfg.WorkDir = t.TempDir()
		cfg.Images = []Image{image}
		if _, err := New(cfg); !errors.Is(err, execenv.ErrInvalid) {
			t.Errorf("%s: New() error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestReadyReportsIsolationClass(t *testing.T) {
	t.Parallel()
	image := writeCatalogImage(t, t.TempDir(), "default", "root")
	for _, class := range []execenv.Isolation{"", execenv.IsolationVM, execenv.IsolationUserKernel} {
		host, err := New(Config{WorkDir: t.TempDir(), Images: []Image{image}, Isolation: class})
		if err != nil {
			t.Fatal(err)
		}
		report, err := host.Ready(t.Context())
		_ = host.lock.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := class
		if want == "" {
			want = execenv.IsolationVM
		}
		if report.Isolation != want {
			t.Fatalf("Ready().Isolation = %q, want %q", report.Isolation, want)
		}
		if !host.Capabilities().Isolated || execenv.RequireIsolated(host) != nil {
			t.Fatalf("%s host is not isolated", want)
		}
		_, isSandbox := host.launch.(*sandboxLauncher)
		if isSandbox != (want == execenv.IsolationUserKernel) {
			t.Fatalf("%s host chose the wrong launcher %T", want, host.launch)
		}
	}
}

// The host logic above the launcher is shared, so a user-kernel host must
// satisfy the same conformance suite. The live suite with the real sandbox
// runtime is in isolation_test.go.
func TestConformanceUserKernelHost(t *testing.T) {
	execenvtest.Run(t, func(t *testing.T) execenv.Host {
		t.Helper()
		dir := t.TempDir()
		h, err := New(Config{
			Isolation: execenv.IsolationUserKernel,
			WorkDir:   t.TempDir(),
			Slots:     2,
			Images: []Image{
				writeCatalogImage(t, dir, "default", "root-default"),
				writeCatalogImage(t, dir, "alt", "root-alt"),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		h.probe = func() error { return nil }
		h.launch = &recordingLauncher{}
		h.attach = &recordingAttacher{}
		t.Cleanup(func() { _ = h.lock.Close() })
		return h
	})
}

func TestSandboxSpecMatchesGuestContract(t *testing.T) {
	t.Parallel()
	req := startRequest{ID: "grant-1", Memory: 256 << 20, CPU: 1500}
	spec := sandboxSpec(req, "/images/x/root", "/grants/g/disk")
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Root struct {
			Path     string `json:"path"`
			Readonly bool   `json:"readonly"`
		} `json:"root"`
		Process struct {
			User struct {
				UID int `json:"uid"`
				GID int `json:"gid"`
			} `json:"user"`
			Cwd          string              `json:"cwd"`
			Args         []string            `json:"args"`
			Env          []string            `json:"env"`
			NoNewPrivs   bool                `json:"noNewPrivileges"`
			Capabilities map[string][]string `json:"capabilities"`
			Terminal     bool                `json:"terminal"`
		} `json:"process"`
		Mounts []struct {
			Destination string `json:"destination"`
			Type        string `json:"type"`
			Source      string `json:"source"`
		} `json:"mounts"`
		Linux struct {
			Namespaces []struct {
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"namespaces"`
			Resources struct {
				Memory struct {
					Limit int64 `json:"limit"`
				} `json:"memory"`
				CPU struct {
					Quota  int64 `json:"quota"`
					Period int64 `json:"period"`
				} `json:"cpu"`
			} `json:"resources"`
		} `json:"linux"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Root.Path != "/images/x/root" || !doc.Root.Readonly {
		t.Fatalf("root = %+v", doc.Root)
	}
	p := doc.Process
	if p.User.UID != sandboxUID || p.User.GID != sandboxUID || !p.NoNewPrivs || p.Terminal || p.Cwd != execenv.GuestWorkspace {
		t.Fatalf("process = %+v", p)
	}
	for set, caps := range p.Capabilities {
		if len(caps) != 0 {
			t.Fatalf("capability set %s = %v", set, caps)
		}
	}
	wantArgs := []string{execenv.GuestBin, "guest-init", "-home", execenv.GuestWorkspace, "-scratch", execenv.GuestHome, "-listen", "stdio"}
	if !slices.Equal(p.Args, wantArgs) {
		t.Fatalf("args = %v", p.Args)
	}
	if !slices.Contains(p.Env, "HOME="+execenv.GuestHome) {
		t.Fatalf("env = %v", p.Env)
	}
	mounts := map[string]string{}
	for _, m := range doc.Mounts {
		mounts[m.Destination] = m.Type + ":" + m.Source
	}
	if mounts[execenv.GuestWorkspace] != "bind:/grants/g/disk/"+diskWorkspace || mounts[execenv.GuestHome] != "bind:/grants/g/disk/"+diskHome {
		t.Fatalf("mounts = %v", mounts)
	}
	if !strings.HasPrefix(mounts["/dev/pts"], "devpts") {
		t.Fatalf("no pty filesystem: %v", mounts)
	}
	if doc.Linux.Resources.Memory.Limit != 256<<20 || doc.Linux.Resources.CPU.Quota != 150000 || doc.Linux.Resources.CPU.Period != 100000 {
		t.Fatalf("resources = %+v", doc.Linux.Resources)
	}
	for _, ns := range doc.Linux.Namespaces {
		if ns.Type == "network" && ns.Path != "" {
			t.Fatalf("network none joined a namespace: %+v", ns)
		}
	}
	allow := sandboxSpec(startRequest{ID: "grant-1", Attach: &netAttach{NetNS: "/var/run/netns/execenv-e1"}}, "/r", "/d")
	found := false
	for _, ns := range allow["linux"].(map[string]any)["namespaces"].([]map[string]any) {
		if ns["type"] == "network" && ns["path"] == "/var/run/netns/execenv-e1" {
			found = true
		}
	}
	if !found {
		t.Fatal("allowlist sandbox does not join its network namespace")
	}
	defaults := sandboxSpec(startRequest{ID: "grant-1"}, "/r", "/d")
	if got := defaults["linux"].(map[string]any)["resources"].(map[string]any)["memory"].(map[string]any)["limit"]; got != int64(sandboxMemory) {
		t.Fatalf("default memory = %v", got)
	}
}

func TestSandboxRuntimeArgs(t *testing.T) {
	t.Parallel()
	l := newSandboxLauncher(Config{WorkDir: "/w"})
	none := strings.Join(l.runtimeArgs(false, "run", "x"), " ")
	if none != "--root /w/"+sandboxState+" --network=none --overlay2=none run x" {
		t.Fatalf("args = %q", none)
	}
	if !strings.Contains(strings.Join(l.runtimeArgs(true), " "), "--network=sandbox") {
		t.Fatal("allowlist grant has no sandbox network")
	}
}

func TestSandboxIDIsStableAndOpaque(t *testing.T) {
	t.Parallel()
	a, b := sandboxID("Grant With Spaces/../x"), sandboxID("Grant With Spaces/../x")
	if a != b || strings.ContainsAny(a, " /.") || !strings.HasPrefix(a, "execenv-") || sandboxID("other") == a {
		t.Fatalf("sandboxID = %q", a)
	}
}

func TestSandboxStatus(t *testing.T) {
	t.Parallel()
	if s, err := sandboxStatus([]byte(`{"id":"x","status":"paused"}`)); err != nil || s != "paused" {
		t.Fatalf("status = %q, %v", s, err)
	}
	if _, err := sandboxStatus([]byte(`not json`)); err == nil {
		t.Fatal("garbage state accepted")
	}
}

func TestSandboxConnectHandsOutOneLink(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	inst := &sandboxInstance{conn: pipeConnForTest(r, w)}
	if _, err := inst.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Connect(t.Context()); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("second Connect error = %v", err)
	}
}
