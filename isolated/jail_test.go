package isolated

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestJailIDIsAcceptedBySupervisor(t *testing.T) {
	t.Parallel()
	valid := regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	for _, id := range []execenv.ID{"grant-1", "job.7f3a:attempt_2", "a"} {
		if got := jailID(id); !valid.MatchString(got) {
			t.Fatalf("jailID(%q) = %q", id, got)
		}
	}
	if jailID("a") == jailID("b") || jailID("a") != jailID("a") {
		t.Fatal("jail ids are not stable and distinct")
	}
}

func TestJailLayoutFollowsSupervisorConvention(t *testing.T) {
	t.Parallel()
	j := newJailLayout("/w/jails", "/usr/local/bin/firecracker", "grant-1", 7, 7)
	want := filepath.Join("/w/jails", "firecracker", jailID("grant-1"), "root")
	if j.root != want || j.dir != filepath.Dir(want) || j.uid != 7 {
		t.Fatalf("layout = %+v, want root %s", j, want)
	}
}

func TestJailPopulatePlacesArtifactsInsideRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := startRequest{
		ID:     "grant-1",
		Kernel: filepath.Join(dir, "images", "vmlinux"),
		Rootfs: filepath.Join(dir, "images", "rootfs-python.ext4"),
		Disk:   filepath.Join(dir, "grants", "grant-1", diskName),
	}
	for _, path := range []string{req.Kernel, req.Rootfs, req.Disk} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jail := newJailLayout(filepath.Join(dir, jailsDir), "/bin/firecracker", req.ID, os.Getuid(), os.Getgid())
	if err := jail.populate(req); err != nil {
		t.Fatal(err)
	}
	for src, name := range map[string]string{req.Kernel: jailKernel, req.Rootfs: jailRootfs, req.Disk: jailDisk} {
		a, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.Stat(jail.host(name))
		if err != nil {
			t.Fatalf("%s missing from jail: %v", name, err)
		}
		if !os.SameFile(a, b) {
			t.Fatalf("%s in the jail is not the host file", name)
		}
	}
	// Removing the jail removes only its own names.
	if err := jail.remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jail.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("jail survived removal: %v", err)
	}
	for _, path := range []string{req.Kernel, req.Rootfs, req.Disk} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("jail removal touched %s: %v", path, err)
		}
	}
}

func TestJailUIDsAreDistinctPerGrant(t *testing.T) {
	t.Parallel()
	pool := newJailUIDs(0)
	a, err := pool.acquire()
	if err != nil {
		t.Fatal(err)
	}
	b, err := pool.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a != defaultJailUIDBase || !pool.contains(b) || pool.contains(defaultJailUIDBase-1) || pool.contains(defaultJailUIDBase+jailUIDCount) {
		t.Fatalf("uids = %d, %d", a, b)
	}
	pool.release(a)
	if again, _ := pool.acquire(); again != a {
		t.Fatalf("released uid not reused: %d", again)
	}
	if got := newJailUIDs(5000).base; got != 5000 {
		t.Fatalf("configured base = %d", got)
	}
}

func testJail(t *testing.T, dir string) jailLayout {
	t.Helper()
	jail := newJailLayout(filepath.Join(dir, jailsDir), "/bin/firecracker", "g1", os.Getuid(), os.Getgid())
	if err := os.MkdirAll(jail.root, 0o700); err != nil {
		t.Fatal(err)
	}
	return jail
}
