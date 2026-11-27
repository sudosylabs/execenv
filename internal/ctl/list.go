package ctl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sudosylabs/execenv/daemon"
)

// List prints ids installed on this host and ids the index currently
// publishes. It does not print tokens or hashes.
func List(opts Options, stdout io.Writer) error {
	opts = opts.resolved()
	installed := installedIDs(opts)
	if stdout != nil {
		fmt.Fprintf(stdout, "installed=%s\n", joinIDs(installed))
	}
	available, err := availableIDs(opts)
	if err != nil {
		if stdout != nil {
			fmt.Fprintln(stdout, "available=unavailable")
		}
		return wrap("list", err)
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "available=%s\n", joinIDs(available))
	}
	return nil
}

func installedIDs(opts Options) []string {
	doc, err := loadHost(opts.configPath())
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(doc.Images))
	for _, img := range doc.Images {
		if img.ID != "" {
			out = append(out, img.ID)
		}
	}
	return out
}

func availableIDs(opts Options) ([]string, error) {
	base, err := opts.releaseBase()
	if err != nil {
		return nil, err
	}
	raw, err := fetchBytes(base + "/index.json")
	if err != nil {
		return nil, err
	}
	idx, err := parseIndex(raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(idx.Images))
	for _, img := range idx.Images {
		if img.ID != "" {
			out = append(out, img.ID)
		}
	}
	return out, nil
}

func joinIDs(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ",")
}

// Remove deletes one catalog id from this host. Other ids stay. Removing
// an id that is not installed is success. Shared files stay while another
// image still references them.
func Remove(opts Options, id string, stdout io.Writer) error {
	opts = opts.resolved()
	if id == "" {
		return wrap("remove", fmt.Errorf("missing id"))
	}
	if _, err := os.Stat(opts.Sysconf); errors.Is(err, os.ErrNotExist) {
		if stdout != nil {
			fmt.Fprintf(stdout, "removed=%s\n", id)
		}
		return nil
	}
	lock, err := opts.lockConfig()
	if err != nil {
		return wrap("remove", fmt.Errorf("lock host configuration: %w", err))
	}
	defer lock.Close()
	doc, err := loadHost(opts.configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if stdout != nil {
				fmt.Fprintf(stdout, "removed=%s\n", id)
			}
			return nil
		}
		return err
	}
	token := doc.Token
	var kept []daemon.Image
	var dropped *daemon.Image
	for i := range doc.Images {
		img := doc.Images[i]
		if img.ID == id {
			copy := img
			dropped = &copy
			continue
		}
		kept = append(kept, img)
	}
	if dropped != nil {
		finish, err := opts.guardRemoval(id)
		if err != nil {
			return wrap("remove", err)
		}
		defer finish()
		original := doc
		doc.Images = kept
		if err := daemon.Save(opts.configPath(), doc); err != nil {
			return err
		}
		again, err := loadHost(opts.configPath())
		if err != nil {
			return err
		}
		if again.Token != token {
			return wrap("remove", fmt.Errorf("host token changed"))
		}
		if err := opts.reloadUnit(); err != nil {
			restoreErr := daemon.Save(opts.configPath(), original)
			return wrap("remove", errors.Join(fmt.Errorf("restart after removing %s: %w", id, err), restoreErr))
		}
		for _, name := range []string{dropped.Rootfs, dropped.Kernel} {
			if !imageFileInUse(kept, name) {
				if err := removeFile(name); err != nil {
					return err
				}
			}
		}
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "removed=%s\n", id)
	}
	return nil
}

// guardRemoval holds admission closed on a durable daemon while its
// configuration is replaced. Existing grants keep using their own images.
func (o Options) guardRemoval(id string) (func(), error) {
	done := func() {}
	reply, err := o.operate(daemon.OperatorStatus)
	if !o.Force {
		if verdict := restartVerdict(o.unitActive(), reply, err); verdict != nil {
			return nil, verdict
		}
		if err != nil {
			if _, offlineErr := o.checkUpgrade(false); offlineErr != nil {
				return nil, offlineErr
			}
		}
	}
	if err != nil || !reply.Durable {
		return done, nil
	}
	if imageInUse(reply, id) {
		return nil, fmt.Errorf("%w: live grants were started from %s; revoke them first", errImageInUse, id)
	}
	if reply.State == daemon.DrainServing {
		reply, err = o.operate(daemon.OperatorDrain)
		done = func() { _, _ = o.operate(daemon.OperatorUndrain) }
		if err != nil {
			done()
			return nil, err
		}
		if imageInUse(reply, id) {
			done()
			return nil, fmt.Errorf("%w: live grants were started from %s; revoke them first", errImageInUse, id)
		}
	}
	return done, nil
}

func removeFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return wrap("remove", err)
	}
	return nil
}

func imageFileInUse(images []daemon.Image, name string) bool {
	info, err := os.Stat(name)
	for _, img := range images {
		for _, current := range []string{img.Kernel, img.Rootfs} {
			if filepath.Clean(current) == filepath.Clean(name) {
				return true
			}
			if candidate, statErr := os.Stat(current); err == nil && statErr == nil && os.SameFile(info, candidate) {
				return true
			}
		}
	}
	return false
}
