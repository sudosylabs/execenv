package isolated

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/start"
)

// startTimeout bounds how long a booted or restored guest may take to
// reach its start message.
const startTimeout = 60 * time.Second

// templateDiskBytes is the placeholder disk a template is built with. The
// guest never mounts it: init waits for a start before it touches a disk,
// and each grant's own disk takes its place.
const templateDiskBytes = 64 << 20

// restoreFrom is a saved machine a grant starts from instead of booting.
type restoreFrom struct {
	// Dir holds the saved state and memory.
	Dir string
	// Start says the guest waits for a start message: true for a
	// template, false for a suspended grant, which carries on where it
	// stopped.
	Start bool
	// Own hands the files to the grant's jail identity: a suspended
	// grant's are its own, a template's are shared and read only.
	Own bool
}

// placeSaved links a saved machine's files into the jail root, where the
// monitor loads them.
func (j jailLayout) placeSaved(r *restoreFrom) error {
	for _, name := range []string{jailState, jailMemory} {
		dst := j.host(name)
		if err := os.Link(filepath.Join(r.Dir, name), dst); err != nil {
			return err
		}
		if r.Own {
			if err := os.Chown(dst, j.uid, j.gid); err != nil {
				return err
			}
		}
	}
	return nil
}

// bring makes a started machine a running guest: a restored one is loaded,
// shown its own disk and resumed; then a guest that waits for a start is
// given one.
func (l *processLauncher) bring(ctx context.Context, req startRequest, p *processInstance) error {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if req.Restore != nil {
		if err := waitSocket(ctx, p.api); err != nil {
			return err
		}
		m := newMonitor(p.api)
		defer m.close()
		dev := ""
		if req.Attach != nil {
			dev = req.Attach.Dev
		}
		if err := m.load(ctx, dev); err != nil {
			return err
		}
		if err := m.rescanDisk(ctx); err != nil {
			return err
		}
		// The saved machine had its own limits, or none: these are the
		// grant's.
		if err := p.UpdateLimits(ctx, req.Limits, req.Attach != nil); err != nil {
			return err
		}
		if err := m.resumeMachine(ctx); err != nil {
			return err
		}
	}
	if req.Epoch == "" || (req.Restore != nil && !req.Restore.Start) {
		return nil
	}
	msg, err := start.New(req.Epoch, startNet(req.Attach))
	if err != nil {
		return err
	}
	return untilDone(ctx, func() error {
		conn, err := dialGuest(ctx, p.sock, start.Port)
		if err != nil {
			return err
		}
		defer conn.Close()
		return start.Send(conn, msg)
	})
}

// startNet is the network a guest is started with: its link's addresses.
func startNet(a *netAttach) *start.Net {
	if a == nil {
		return nil
	}
	return &start.Net{Address: a.GuestIP, Prefix: a.Prefix, Gateway: a.HostIP}
}

// untilDone retries fn until it succeeds or ctx ends: the guest is not
// listening the moment its machine starts.
func untilDone(ctx context.Context, fn func() error) error {
	for {
		err := fn()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(execenv.ErrUnavailable, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Save writes the running machine's state and memory into dir, mode 0600,
// leaving the machine paused. The grant's agent link breaks with it.
func (p *processInstance) Save(ctx context.Context, dir string) error {
	// A signal-stopped monitor cannot answer its API.
	_ = signalProcess(ctx, p.pid, false)
	m := newMonitor(p.api)
	defer m.close()
	if err := m.pauseMachine(ctx); err != nil {
		_ = signalProcess(ctx, p.pid, true)
		return err
	}
	err := m.save(ctx)
	if err == nil {
		err = takeSaved(filepath.Join(p.rec.Jail, "root"), dir, 0o600)
	}
	if err != nil {
		// Not saved: the machine stays as it was, frozen.
		_ = m.resumeMachine(ctx)
		_ = signalProcess(ctx, p.pid, true)
		return err
	}
	return nil
}

// takeSaved moves a saved machine's files out of a jail root.
func takeSaved(root, dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{jailState, jailMemory} {
		dst := filepath.Join(dir, name)
		if err := os.Rename(filepath.Join(root, name), dst); err != nil {
			return err
		}
		if err := os.Chown(dst, 0, 0); err != nil {
			return err
		}
		if err := os.Chmod(dst, mode); err != nil {
			return err
		}
	}
	return nil
}

// templateMeta names what a template was built from. A template whose
// image or shape no longer matches is built again.
type templateMeta struct {
	Image string `json:"image"`
	Hash  string `json:"hash"`
	Shape Shape  `json:"shape"`
	// Net is set for a template saved with a network device.
	Net bool `json:"net,omitempty"`
}

const templateMetaName = "template.json"

// buildTemplate boots image as shape with a placeholder disk, and with
// network device att when it is set, waits until its guest is idle,
// waiting for a start, then saves it into dir. The guest has no address
// yet: each grant's start gives it its own, and its restore its own
// device. Files are shared by every grant restored from it, so they are
// root's and read only.
func (l *processLauncher) buildTemplate(ctx context.Context, image Image, shape Shape, att *netAttach, dir string) error {
	work := dir + ".build"
	if err := os.RemoveAll(work); err != nil {
		return err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(work)
	req := startRequest{
		ID:        (templateKey{image.ID, shape.Name, att != nil}).identity(),
		Kernel:    image.Kernel,
		Rootfs:    image.Rootfs,
		TreeDir:   filepath.Join(work, workspaceName),
		Disk:      filepath.Join(work, diskName),
		Resources: execenv.Resources{CPUMillis: int64(shape.VCPUs) * 1000, MemoryBytes: shape.MemoryBytes, DiskBytes: templateDiskBytes},
		Attach:    att,
	}
	inst, err := l.Start(ctx, req)
	if err != nil {
		if inst != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return errors.Join(err, inst.Stop(stopCtx))
		}
		return err
	}
	p := inst.(*processInstance)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = p.Stop(stopCtx)
		cancel()
	}()
	wait, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if err := untilDone(wait, func() error {
		conn, err := dialGuest(wait, p.sock, start.Port)
		if err != nil {
			return err
		}
		defer conn.Close()
		return start.Probe(conn)
	}); err != nil {
		return err
	}
	m := newMonitor(p.api)
	defer m.close()
	if err := m.pauseMachine(ctx); err != nil {
		return err
	}
	if err := m.save(ctx); err != nil {
		return err
	}
	saved := filepath.Join(work, "saved")
	if err := takeSaved(filepath.Join(p.rec.Jail, "root"), saved, 0o644); err != nil {
		return err
	}
	meta, err := json.Marshal(templateMeta{Image: string(image.ID), Hash: image.Hash, Shape: shape, Net: att != nil})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(saved, templateMetaName), meta, 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(saved, dir)
}

// readTemplate reports whether dir holds a template of image and shape,
// saved with a network device when net is set.
func readTemplate(dir string, image Image, shape Shape, net bool) bool {
	raw, err := os.ReadFile(filepath.Join(dir, templateMetaName))
	if err != nil {
		return false
	}
	var meta templateMeta
	if json.Unmarshal(raw, &meta) != nil {
		return false
	}
	if meta.Image != string(image.ID) || meta.Hash != image.Hash || meta.Shape != shape || meta.Net != net {
		return false
	}
	for _, name := range []string{jailState, jailMemory} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}
