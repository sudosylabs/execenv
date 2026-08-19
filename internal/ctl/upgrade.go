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

// Upgrade replaces execenvctl and execenv from the release channel.
// Catalog disks, the host token, and TLS files are not touched.
func Upgrade(opts Options, stdout io.Writer) error {
	opts = opts.resolved()
	if err := os.MkdirAll(opts.binDir(), 0o755); err != nil {
		return wrap("upgrade", err)
	}
	base, err := opts.releaseBase()
	if err != nil {
		return err
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
	if len(replaced) > 0 {
		if err := opts.reloadUnit(); err != nil {
			return err
		}
	}
	if stdout != nil {
		if len(replaced) == 0 {
			fmt.Fprintln(stdout, "upgraded=already current")
		} else {
			fmt.Fprintf(stdout, "upgraded=%s\n", strings.Join(replaced, ","))
		}
	}
	return nil
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
