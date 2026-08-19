package ctl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/daemon"
	"github.com/sudosylabs/execenv/isolated"
)

// Install fetches one catalog id from the release channel onto an already
// bootstrapped host. It does not occupy grants and does not pull at
// Ensure time. The host token is left untouched.
func Install(opts Options, id string, stdout io.Writer) error {
	opts = opts.resolved()
	if id == "" {
		return wrap("install", execenv.ErrInvalid)
	}
	doc, err := loadHost(opts.configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return wrap("install", fmt.Errorf("host config missing; run execenvctl bootstrap first"))
		}
		return err
	}
	token := doc.Token
	raw, err := fetchBytes(opts.releaseBase() + "/index.json")
	if err != nil {
		return err
	}
	idx, err := parseIndex(raw)
	if err != nil {
		return err
	}
	entry, err := idx.lookup(id)
	if err != nil {
		return err
	}
	imageDir := filepath.Join(opts.State, "images")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return wrap("install", err)
	}
	kernelDest := filepath.Join(imageDir, filepath.Base(idx.Kernel))
	rootfsDest := filepath.Join(imageDir, filepath.Base(entry.Rootfs))
	kernel, rootfs, err := fetchVerified(opts.releaseBase(), idx.Kernel, entry.Rootfs, kernelDest, rootfsDest, entry.Hash)
	if err != nil {
		return err
	}
	upsertImage(&doc, daemon.Image{
		ID:     entry.ID,
		Kernel: kernel,
		Rootfs: rootfs,
		Hash:   entry.Hash,
	})
	if err := daemon.Save(opts.configPath(), doc); err != nil {
		return err
	}
	again, err := loadHost(opts.configPath())
	if err != nil {
		return err
	}
	if again.Token != token {
		return wrap("install", fmt.Errorf("host token changed"))
	}
	if err := opts.reloadUnit(); err != nil {
		return err
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "installed=%s\n", entry.ID)
		fmt.Fprintln(stdout, "hash=written to config")
	}
	return nil
}

// fetchVerified downloads into sibling .new files, checks the catalog
// hash, then renames into place. A bad download cannot replace a good disk.
func fetchVerified(base, kernelName, rootfsName, kernelDest, rootfsDest, want string) (string, string, error) {
	kernel := kernelDest
	if !fileExists(kernelDest) {
		kernel = kernelDest + ".new"
		if err := fetchURL(base+"/"+filepath.Base(kernelName), kernel); err != nil {
			return "", "", err
		}
	}
	rootfs := rootfsDest + ".new"
	if err := fetchURL(base+"/"+filepath.Base(rootfsName), rootfs); err != nil {
		if kernel != kernelDest {
			_ = os.Remove(kernel)
		}
		return "", "", err
	}
	sum, err := isolated.Digest(kernel, rootfs)
	if err != nil || sum != want {
		_ = os.Remove(rootfs)
		if kernel != kernelDest {
			_ = os.Remove(kernel)
		}
		if err != nil {
			return "", "", wrap("install", err)
		}
		return "", "", wrap("install", fmt.Errorf("catalog hash does not match kernel+rootfs on disk"))
	}
	if kernel != kernelDest {
		if err := os.Rename(kernel, kernelDest); err != nil {
			_ = os.Remove(kernel)
			_ = os.Remove(rootfs)
			return "", "", wrap("install", err)
		}
		kernel = kernelDest
	}
	if err := os.Rename(rootfs, rootfsDest); err != nil {
		_ = os.Remove(rootfs)
		return "", "", wrap("install", err)
	}
	return kernel, rootfsDest, nil
}

func (o Options) reloadUnit() error {
	if o.Reload != nil {
		return o.Reload()
	}
	return restartActiveUnit()
}

func restartActiveUnit() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	if err := exec.Command("systemctl", "is-active", "--quiet", unitName).Run(); err != nil {
		return nil
	}
	out, err := exec.Command("systemctl", "restart", unitName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart: %w", errTrim(out, err))
	}
	return nil
}
