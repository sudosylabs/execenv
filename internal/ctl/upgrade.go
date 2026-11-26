package ctl

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sudosylabs/execenv/daemon"
	"github.com/sudosylabs/execenv/isolated"
)

const checksumName = "SHA256SUMS"

var upgradeNames = []string{execenvName, "execenvctl"}

// Upgrade replaces execenvctl and execenv from this process's tagged
// release, reinstalls every catalog id already on the host so the baked
// agent matches, then restarts a running daemon once. The host token and
// TLS files are not touched. An upgrade requires an empty host because
// releases can change the guest protocol or the boot artifacts.
func Upgrade(opts Options, stdout io.Writer) error {
	opts = opts.resolved()
	if _, err := opts.checkUpgrade(false); err != nil {
		return wrap("upgrade", err)
	}
	if err := os.MkdirAll(opts.binDir(), 0o755); err != nil {
		return wrap("upgrade", err)
	}
	base, err := opts.releaseBase()
	if err != nil {
		return err
	}
	idxRaw, err := fetchBytes(base + "/index.json")
	if err != nil {
		return err
	}
	idx, err := parseIndex(idxRaw)
	if err != nil {
		return err
	}
	doc, err := loadHost(opts.configPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, image := range doc.Images {
		if _, err := idx.lookup(image.ID); err != nil {
			return wrap("upgrade", fmt.Errorf("installed image %s missing from this release", image.ID))
		}
	}
	sumsRaw, err := fetchBytes(base + "/" + checksumName)
	if err != nil {
		return err
	}
	sums, err := parseChecksums(sumsRaw)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(opts.binDir(), ".upgrade-")
	if err != nil {
		return wrap("upgrade", err)
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()
	var need []stagedFile
	for _, name := range upgradeNames {
		want, ok := sums[name]
		if !ok {
			return wrap("upgrade", fmt.Errorf("%s missing from %s", name, checksumName))
		}
		dest := filepath.Join(opts.binDir(), name)
		if fileMatches(dest, want) {
			continue
		}
		need = append(need, stagedFile{name: name, dest: dest, tmp: filepath.Join(stage, name), want: want})
	}
	for _, item := range need {
		if err := fetchURL(base+"/"+item.name, item.tmp); err != nil {
			return err
		}
		if err := verifyChecksum(item.tmp, item.want); err != nil {
			return err
		}
		if err := requireLinuxAMD64(item.tmp); err != nil {
			return err
		}
		if err := os.Chmod(item.tmp, 0o755); err != nil {
			return wrap("upgrade", err)
		}
	}
	var images []string
	for i, current := range doc.Images {
		entry, _ := idx.lookup(current.ID)
		if current.Hash == entry.Hash {
			if got, err := isolated.Digest(current.Kernel, current.Rootfs); err == nil && got == entry.Hash {
				continue
			}
		}
		next, err := prepareUpgradeImage(opts, base, idx.Kernel, entry.ID, entry.Rootfs, entry.Hash)
		if err != nil {
			return err
		}
		doc.Images[i] = next
		images = append(images, entry.ID)
	}
	marker := filepath.Join(opts.binDir(), ".execenv-restart-pending")
	_, err = os.Stat(marker)
	pending := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return wrap("upgrade", err)
	}
	if len(need) == 0 && len(images) == 0 && !pending {
		if stdout != nil {
			fmt.Fprintln(stdout, "upgraded=already current")
		}
		return nil
	}
	drained, err := opts.checkUpgrade(true)
	if err != nil {
		if drained && !pending {
			_, _ = opts.operate(daemon.OperatorUndrain)
		}
		return wrap("upgrade", err)
	}
	if drained {
		defer func() {
			// A pending publication or restart must remain drained.
			if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
				_, _ = opts.operate(daemon.OperatorUndrain)
			}
		}()
	}
	for i := range need {
		item := &need[i]
		backup := item.tmp + ".before"
		if err := os.Link(item.dest, backup); err == nil {
			item.backup = backup
		} else if !errors.Is(err, os.ErrNotExist) {
			return wrap("upgrade", err)
		}
	}
	if !pending {
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			return wrap("upgrade", err)
		}
	}
	published := 0
	restore := func(err error) error {
		rollback := restoreBinaries(need[:published])
		keepStage = rollback != nil
		if rollback == nil && !pending {
			rollback = os.Remove(marker)
		}
		return wrap("upgrade", errors.Join(err, rollback))
	}
	var replaced []string
	for _, item := range need {
		if err := os.Rename(item.tmp, item.dest); err != nil {
			return restore(err)
		}
		published++
		replaced = append(replaced, item.name)
	}
	if len(images) > 0 {
		if err := daemon.Save(opts.configPath(), doc); err != nil {
			return restore(err)
		}
	}
	if err := opts.reloadUnit(); err != nil {
		return wrap("upgrade", fmt.Errorf("files replaced but the daemon was not restarted: %w", err))
	}
	if err := os.Remove(marker); err != nil {
		return wrap("upgrade", err)
	}
	if stdout != nil {
		if len(replaced)+len(images) == 0 {
			fmt.Fprintln(stdout, "upgraded=restarted")
		} else {
			fmt.Fprintf(stdout, "upgraded=%s\n", strings.Join(append(replaced, images...), ","))
		}
	}
	return nil
}

// checkUpgrade requires a drained host at publication. It reports whether
// this invocation closed admission so its caller can reopen it on failure.
func (o Options) checkUpgrade(drain bool) (bool, error) {
	if o.Force {
		return false, nil
	}
	reply, err := o.operate(daemon.OperatorStatus)
	if err != nil {
		if o.unitActive() {
			return false, restartVerdict(true, reply, err)
		}
		work := o.workDir()
		if cfg, err := loadHost(o.configPath()); err == nil && cfg.WorkDir != "" {
			work = cfg.WorkDir
		}
		entries, err := os.ReadDir(filepath.Join(work, "grants"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if len(entries) > 0 {
			return false, fmt.Errorf("%w: start the daemon and drain its recorded grants before upgrade", errGrantsLive)
		}
		return false, nil
	}
	if reply.Grants != 0 {
		return false, fmt.Errorf("%w: drain the host before upgrade", errGrantsLive)
	}
	if !drain || reply.State == daemon.DrainDrained {
		return false, nil
	}
	reply, err = o.operate(daemon.OperatorDrain)
	if err != nil {
		return true, err
	}
	if reply.Grants != 0 || reply.State != daemon.DrainDrained {
		return true, fmt.Errorf("%w: wait for the daemon to drain before upgrade", errGrantsLive)
	}
	return true, nil
}

// prepareUpgradeImage verifies a release pair before publishing its directory.
// The current configuration and any older pair remain usable while staging.
func prepareUpgradeImage(opts Options, base, kernelName, id, rootfsName, hash string) (daemon.Image, error) {
	dir := filepath.Join(opts.State, "images")
	dest := filepath.Join(dir, hash)
	image := daemon.Image{ID: id, Kernel: filepath.Join(dest, "kernel"), Rootfs: filepath.Join(dest, "rootfs"), Hash: hash}
	if got, err := isolated.Digest(image.Kernel, image.Rootfs); err == nil && got == hash {
		return image, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return daemon.Image{}, wrap("upgrade", err)
	}
	stage, err := os.MkdirTemp(dir, ".upgrade-")
	if err != nil {
		return daemon.Image{}, wrap("upgrade", err)
	}
	defer os.RemoveAll(stage)
	if _, _, err := fetchVerified(base, kernelName, rootfsName, filepath.Join(stage, "kernel"), filepath.Join(stage, "rootfs"), hash); err != nil {
		return daemon.Image{}, err
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return daemon.Image{}, wrap("upgrade", err)
	}
	if err := os.Rename(stage, dest); err != nil {
		return daemon.Image{}, wrap("upgrade", err)
	}
	return image, nil
}

type stagedFile struct {
	name, dest, tmp, want, backup string
}

func restoreBinaries(items []stagedFile) error {
	var err error
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.backup != "" {
			err = errors.Join(err, os.Rename(item.backup, item.dest))
		} else {
			err = errors.Join(err, os.Remove(item.dest))
		}
	}
	return err
}

func parseChecksums(raw []byte) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) != 64 {
			return nil, wrap("upgrade", fmt.Errorf("invalid checksum file"))
		}
		out[filepath.Base(fields[1])] = fields[0]
	}
	if err := sc.Err(); err != nil {
		return nil, wrap("upgrade", err)
	}
	return out, nil
}

func verifyChecksum(path, want string) error {
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if got != want {
		return wrap("upgrade", fmt.Errorf("checksum mismatch"))
	}
	return nil
}

func fileMatches(path, want string) bool {
	got, err := fileSHA256(path)
	return err == nil && got == want
}

func fileSHA256(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", wrap("upgrade", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
