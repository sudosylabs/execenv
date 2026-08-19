package ctl

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const checksumName = "SHA256SUMS"

var upgradeNames = []string{execenvName, "execenvctl"}

// Upgrade replaces execenvctl and execenv from this process's tagged
// release, then reinstalls every catalog id already on the host so the
// baked agent matches. The host token and TLS files are not touched.
func Upgrade(opts Options, stdout io.Writer) error {
	opts = opts.resolved()
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
	ids := installedIDs(opts)
	for _, id := range ids {
		if _, err := idx.lookup(id); err != nil {
			return wrap("upgrade", fmt.Errorf("installed image %s missing from this release", id))
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
		need = append(need, stagedFile{name: name, dest: dest, tmp: dest + ".new", want: want})
	}
	for _, item := range need {
		if err := fetchURL(base+"/"+item.name, item.tmp); err != nil {
			removeStaged(need)
			return err
		}
		if err := verifyChecksum(item.tmp, item.want); err != nil {
			removeStaged(need)
			return err
		}
		if err := requireLinuxAMD64(item.tmp); err != nil {
			removeStaged(need)
			return err
		}
		if err := os.Chmod(item.tmp, 0o755); err != nil {
			removeStaged(need)
			return wrap("upgrade", err)
		}
	}
	replaced := make([]string, 0, len(need))
	for _, item := range need {
		if err := os.Rename(item.tmp, item.dest); err != nil {
			_ = os.Remove(item.tmp)
			return wrap("upgrade", err)
		}
		replaced = append(replaced, item.name)
	}
	if len(replaced) == 0 && !installedDisksDiffer(opts, idx) {
		if stdout != nil {
			fmt.Fprintln(stdout, "upgraded=already current")
		}
		return nil
	}
	if len(replaced) > 0 {
		if err := opts.reloadUnit(); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if err := Install(opts, id, io.Discard); err != nil {
			return err
		}
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "upgraded=%s\n", strings.Join(append(append([]string(nil), replaced...), ids...), ","))
	}
	return nil
}

func installedDisksDiffer(opts Options, idx Index) bool {
	doc, err := loadHost(opts.configPath())
	if err != nil {
		return false
	}
	for _, img := range doc.Images {
		entry, err := idx.lookup(img.ID)
		if err != nil || entry.Hash != img.Hash {
			return true
		}
	}
	return false
}

type stagedFile struct {
	name, dest, tmp, want string
}

func removeStaged(items []stagedFile) {
	for _, item := range items {
		_ = os.Remove(item.tmp)
	}
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
