package isolated

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/sudosylabs/execenv"
)

// The jail supervisor chroots the machine monitor into
// <base>/<exec-file-name>/<jail-id>/root and drops it to an unprivileged
// identity before it reads its config. Every path the monitor opens is
// resolved inside that root, so the launcher places each artifact there
// before starting the supervisor.
const (
	// jailsDir is the supervisor chroot base under the work directory.
	jailsDir = "jails"
	// Paths inside the jail root, as the monitor sees them.
	jailKernel  = "vmlinux"
	jailRootfs  = "rootfs.ext4"
	jailDisk    = diskName
	jailConfig  = "machine.json"
	jailLinkSoc = "guest.sock"
	// defaultJailUIDBase is the first host uid (and gid) given to a jailed
	// monitor. Each live grant holds its own uid so one escaped monitor
	// cannot signal, trace or read another. The range sits far above
	// ordinary and subordinate uid ranges.
	defaultJailUIDBase = 2_000_000_000
	jailUIDCount       = 65536
)

// jailID is the supervisor's identity for a grant. The supervisor accepts
// only letters, digits and hyphens (at most 64); grant ids may also carry
// '.', '_' and ':'.
func jailID(id execenv.ID) string {
	sum := sha256.Sum256([]byte(id))
	return "g" + hex.EncodeToString(sum[:12])
}

// jailLayout is one grant's jail on the host.
type jailLayout struct {
	// dir is <base>/<exec-file-name>/<jail-id>; root is dir/root.
	dir  string
	root string
	id   string
	uid  int
	gid  int
}

func newJailLayout(base, runtimeBin string, id execenv.ID, uid, gid int) jailLayout {
	jid := jailID(id)
	dir := filepath.Join(base, filepath.Base(runtimeBin), jid)
	return jailLayout{dir: dir, root: filepath.Join(dir, "root"), id: jid, uid: uid, gid: gid}
}

func (j jailLayout) host(name string) string { return filepath.Join(j.root, name) }

// inside is the monitor's view of a jail path.
func inside(name string) string { return "/" + name }

// populate places the boot artifacts and the grant's disk in the jail. The
// kernel and catalog rootfs are hard links to the operator's files (read
// only, owned by root); a read-only bind replaces a link that would cross
// filesystems. The workspace disk is a second link to the grant's disk,
// owned by the jail identity because the monitor writes it.
func (j jailLayout) populate(req startRequest) error {
	if err := os.MkdirAll(j.root, 0o700); err != nil {
		return err
	}
	if err := linkOrBind(req.Kernel, j.host(jailKernel)); err != nil {
		return err
	}
	if err := linkOrBind(req.Rootfs, j.host(jailRootfs)); err != nil {
		return err
	}
	// Both names live under the work directory, so this link never
	// crosses filesystems.
	if err := os.Link(req.Disk, j.host(jailDisk)); err != nil {
		return err
	}
	if err := os.Chown(j.host(jailDisk), j.uid, j.gid); err != nil {
		return err
	}
	return nil
}

// remove releases a jail after its monitor has been reaped: binds first,
// so removal can never delete through them into catalog files.
func (j jailLayout) remove() error {
	if err := unmountUnder(j.dir); err != nil {
		return err
	}
	return os.RemoveAll(j.dir)
}

func linkOrBind(src, dst string) error {
	err := os.Link(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		return err
	}
	_ = f.Close()
	if err := bindReadOnly(src, dst); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// jailUIDs hands each live grant a distinct unprivileged identity.
type jailUIDs struct {
	mu   sync.Mutex
	base int
	used map[int]bool
}

func newJailUIDs(base int) *jailUIDs {
	if base <= 0 {
		base = defaultJailUIDBase
	}
	return &jailUIDs{base: base, used: make(map[int]bool)}
}

func (p *jailUIDs) acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range jailUIDCount {
		uid := p.base + i
		if !p.used[uid] {
			p.used[uid] = true
			return uid, nil
		}
	}
	return 0, execenv.ErrCapacity
}

func (p *jailUIDs) release(uid int) {
	p.mu.Lock()
	delete(p.used, uid)
	p.mu.Unlock()
}

func (p *jailUIDs) contains(uid int) bool {
	return uid >= p.base && uid < p.base+jailUIDCount
}
