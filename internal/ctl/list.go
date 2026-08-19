package ctl

import (
	"errors"
	"fmt"
	"io"
	"os"
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
	raw, err := fetchBytes(opts.releaseBase() + "/index.json")
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
// an id that is not installed is success. The shared kernel is kept if
// another image still names it.
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
		if err := removeFile(dropped.Rootfs); err != nil {
			return err
		}
		if !kernelStillUsed(kept, dropped.Kernel) {
			if err := removeFile(dropped.Kernel); err != nil {
				return err
			}
		}
		if err := opts.reloadUnit(); err != nil {
			return err
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

func kernelStillUsed(images []daemon.Image, kernel string) bool {
	for _, img := range images {
		if img.Kernel == kernel {
			return true
		}
	}
	return false
}
