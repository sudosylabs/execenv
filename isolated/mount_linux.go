//go:build linux

package isolated

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// unmountUnder detaches every mount below root, deepest first. It runs
// before a jail tree or a crashed daemon's jails are removed; removing a
// mounted tree would delete files through the mount and then fail.
func unmountUnder(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()
	var points []string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 5 {
			continue
		}
		point := unescapeMount(fields[4])
		if strings.HasPrefix(point, root+string(filepath.Separator)) {
			points = append(points, point)
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	sort.Slice(points, func(i, j int) bool { return len(points[i]) > len(points[j]) })
	for _, point := range points {
		if err := unix.Unmount(point, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return nil
}

// unescapeMount decodes the octal escapes mountinfo uses for spaces,
// tabs, newlines and backslashes.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
