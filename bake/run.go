package bake

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/isolated"
)

type steps struct {
	exportFS func(ctx context.Context, image, dockerfile, dest string) (string, error)
	packFS   func(ctx context.Context, staging, dest string, size int64) error
}

// Run exports a container (or Dockerfile), installs the guest agent, and
// writes kernel, rootfs, and catalog hash under OutDir. It never occupies
// a grant and never talks to a running host.
func Run(ctx context.Context, req Request) (Result, error) {
	return run(ctx, req, defaultSteps())
}

func run(ctx context.Context, req Request, t steps) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	req = applyDefaults(req)
	if err := validate(req); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	staging, err := os.MkdirTemp(req.OutDir, "staging-")
	if err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	defer os.RemoveAll(staging)

	resolved, err := t.exportFS(ctx, req.Source, req.Dockerfile, staging)
	if err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	if err := installGuest(staging, req.Agent); err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	rootfs := filepath.Join(req.OutDir, DefaultRootfsFile)
	size := req.Size
	if size <= 0 {
		size = autoSize(staging)
	}
	if err := t.packFS(ctx, staging, rootfs, size); err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	kernel := filepath.Join(req.OutDir, DefaultKernelFile)
	if err := copyFile(req.Kernel, kernel, 0o644); err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	sum, err := isolated.Digest(kernel, rootfs)
	if err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	source := resolved
	if source == "" {
		source = req.Source
		if req.Dockerfile != "" {
			source = req.Dockerfile
		}
	}
	out := Result{
		ID:      req.ID,
		Kernel:  kernel,
		Rootfs:  rootfs,
		Hash:    sum,
		Source:  source,
		Version: moduleVersion(),
	}
	if err := writeCatalog(req.OutDir, out); err != nil {
		return Result{}, execenv.Error("bake", err)
	}
	return out, nil
}

func validate(req Request) error {
	if req.OutDir == "" || req.Kernel == "" || req.Agent == "" {
		return execenv.Error("bake", execenv.ErrInvalid)
	}
	if req.Dockerfile != "" && req.Source != "" {
		return execenv.Error("bake", execenv.ErrInvalid)
	}
	if err := execenv.ValidateSpec(execenv.Spec{ID: "ok", Image: req.ID}); err != nil {
		return execenv.Error("bake", execenv.ErrInvalid)
	}
	if !fileOK(req.Kernel) || !fileOK(req.Agent) {
		return execenv.Error("bake", execenv.ErrInvalid)
	}
	if req.Dockerfile != "" && !fileOK(req.Dockerfile) {
		return execenv.Error("bake", execenv.ErrInvalid)
	}
	return nil
}

func installGuest(staging, agent string) error {
	bin := filepath.Join(staging, filepath.FromSlash(guestRel(execenv.GuestBin)))
	init := filepath.Join(staging, filepath.FromSlash(guestRel(execenv.GuestInit)))
	home := filepath.Join(staging, filepath.FromSlash(guestRel(execenv.GuestHome)))
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(init), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	if err := copyFile(agent, bin, 0o755); err != nil {
		return err
	}
	return os.WriteFile(init, []byte(guestInit), 0o755)
}

func writeCatalog(dir string, res Result) error {
	doc := struct {
		ID      execenv.Image `json:"id"`
		Kernel  string        `json:"kernel"`
		Rootfs  string        `json:"rootfs"`
		Hash    string        `json:"hash"`
		Source  string        `json:"source"`
		Version string        `json:"execenv,omitempty"`
	}{
		ID:      res.ID,
		Kernel:  DefaultKernelFile,
		Rootfs:  DefaultRootfsFile,
		Hash:    res.Hash,
		Source:  res.Source,
		Version: res.Version,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(filepath.Join(dir, "catalog.json"), raw, 0o644)
}

func autoSize(staging string) int64 {
	var total int64
	_ = filepath.Walk(staging, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		return nil
	})
	need := total*2 + 64<<20
	if need < 64<<20 {
		need = 64 << 20
	}
	return need
}

func copyFile(from, to string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}

func fileOK(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func guestRel(abs string) string {
	return strings.TrimPrefix(abs, "/")
}

func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}
