package ctl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/daemon"
)

// Install fetches one catalog id from the release channel onto an already
// bootstrapped host, then restarts a running daemon so it serves the disk.
// It does not occupy grants and does not pull at Ensure time. The host
// token is left untouched. It refuses, before changing anything, to restart
// a daemon that holds grants unless opts.Force is set.
func Install(opts Options, id string, stdout io.Writer) error {
	opts = opts.resolved()
	if id == "" {
		return wrap("install", execenv.ErrInvalid)
	}
	lock, err := opts.lockConfig()
	if err != nil {
		return wrap("install", fmt.Errorf("lock host configuration: %w", err))
	}
	defer lock.Close()
	if err := opts.guardRestart(); err != nil {
		return wrap("install", err)
	}
	installed, err := installImage(opts, id)
	if err != nil {
		return err
	}
	if err := opts.reloadUnit(); err != nil {
		return wrap("install", fmt.Errorf("%s is in host config but the daemon was not restarted: %w", installed, err))
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "installed=%s\n", installed)
		fmt.Fprintln(stdout, "hash=written to config")
	}
	return nil
}

// installImage fetches, verifies and records one catalog id without
// restarting the daemon. It returns the installed id.
func installImage(opts Options, id string) (string, error) {
	doc, err := loadHost(opts.configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", wrap("install", fmt.Errorf("host config missing; run execenvctl bootstrap first"))
		}
		return "", err
	}
	token := doc.Token
	base, err := opts.releaseBase()
	if err != nil {
		return "", err
	}
	raw, err := fetchBytes(base + "/index.json")
	if err != nil {
		return "", err
	}
	idx, err := parseIndex(raw)
	if err != nil {
		return "", err
	}
	entry, err := idx.lookup(id)
	if err != nil {
		return "", err
	}
	image, err := prepareCatalogImage(opts, base, idx.Kernel, entry.ID, entry.Rootfs, entry.Hash, doc.Images)
	if err != nil {
		return "", wrap("install", err)
	}
	upsertImage(&doc, image)
	if err := daemon.Save(opts.configPath(), doc); err != nil {
		return "", err
	}
	again, err := loadHost(opts.configPath())
	if err != nil {
		return "", err
	}
	if again.Token != token {
		return "", wrap("install", fmt.Errorf("host token changed"))
	}
	return entry.ID, nil
}

// reloadUnit restarts a running daemon, after guardRestart agrees.
func (o Options) reloadUnit() error {
	if err := o.guardRestart(); err != nil {
		return err
	}
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
	return restartUnit()
}

func restartUnit() error {
	out, err := exec.Command("systemctl", "restart", unitName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart: %w", errTrim(out, err))
	}
	return nil
}
