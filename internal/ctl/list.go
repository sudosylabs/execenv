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
		// A live grant runs from the image's disks, so it is never removed
		// from under one; and removing a disk restarts the daemon.
		if reply, err := opts.operate(daemon.OperatorStatus); err == nil && imageInUse(reply, id) {
			return wrap("remove", fmt.Errorf("%w: live grants were started from %s; revoke them first", errImageInUse, id))
		}
		if err := opts.guardRestart(); err != nil {
			return wrap("remove", err)
		}
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
		for _, name := range []string{dropped.Rootfs, dropped.Kernel} {
			if !imageFileInUse(kept, name) {
				if err := removeFile(name); err != nil {
					return err
				}
			}
		}
		if err := opts.reloadUnit(); err != nil {
			return wrap("remove", fmt.Errorf("%s is out of host config but the daemon was not restarted: %w", id, err))
		}
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "removed=%s\n", id)
	}
	return nil
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
