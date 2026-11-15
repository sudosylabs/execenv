package watch

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func event(mask uint64, info []byte) []byte { return eventBy(mask, 0, info) }

func eventBy(mask uint64, pid int32, info []byte) []byte {
	b := make([]byte, unix.FAN_EVENT_METADATA_LEN+len(info))
	binary.NativeEndian.PutUint32(b[0:4], uint32(len(b)))
	b[4] = 3 // version
	binary.NativeEndian.PutUint16(b[6:8], unix.FAN_EVENT_METADATA_LEN)
	binary.NativeEndian.PutUint64(b[8:16], mask)
	binary.NativeEndian.PutUint32(b[16:20], ^uint32(0)) // FAN_NOFD
	binary.NativeEndian.PutUint32(b[20:24], uint32(pid))
	copy(b[unix.FAN_EVENT_METADATA_LEN:], info)
	return b
}

// An overflow, or a record the watcher cannot place, means everything may
// have changed.
func TestParseErrsTowardsWalking(t *testing.T) {
	w := &Watcher{queue: -1, dirty: map[string]struct{}{}}
	w.parse(event(unix.FAN_Q_OVERFLOW, nil))
	if _, all := w.Take(); !all {
		t.Fatal("overflow not reported")
	}
	w.parse(event(unix.FAN_MODIFY, []byte{unix.FAN_EVENT_INFO_TYPE_DFID_NAME, 0, 2, 0}))
	if _, all := w.Take(); !all {
		t.Fatal("unplaceable record not reported")
	}
	w.parse([]byte{1, 2, 3})
	if paths, all := w.Take(); all || len(paths) != 0 {
		t.Fatal("a short read invented a change")
	}
}

// The last writer of a path names its tag; an unknown writer after a known
// one clears it.
func TestMarkKeepsTheLastWritersTag(t *testing.T) {
	tags := map[int]string{10: "build", 11: "shell"}
	w := &Watcher{queue: -1, dirty: map[string]struct{}{}, writer: func(pid int) string { return tags[pid] }}
	w.mark("a", 10)
	w.mark("a", 11)
	w.mark("b", 10)
	w.mark("c", 10)
	w.mark("c", 99)
	if paths, all := w.Take(); all || len(paths) != 3 {
		t.Fatalf("Take() = %v, %v", paths, all)
	}
	writers := w.Writers()
	if writers["a"] != "shell" || writers["b"] != "build" || writers["c"] != "" {
		t.Fatalf("Writers() = %v", writers)
	}
	if again := w.Writers(); len(again) != 0 {
		t.Fatalf("Writers() kept %v", again)
	}
}

// Live: needs root and fanotify. A change in the workspace is reported by
// its path, at any depth; one outside it on the same filesystem is not.
func TestWatcherNamesWorkspaceChanges(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("fanotify filesystem marks need root")
	}
	base := t.TempDir()
	home := filepath.Join(base, "workspace")
	other := filepath.Join(base, "other")
	for _, d := range []string{home, other, filepath.Join(home, "d")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w, err := New(home, nil)
	if err != nil {
		t.Skipf("fanotify unavailable: %v", err)
	}
	defer w.Close()
	if _, all := w.Take(); !all {
		t.Fatal("a new watcher must start by asking for a walk")
	}
	if err := os.WriteFile(filepath.Join(other, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if paths, all := w.Take(); all || len(paths) != 0 {
		t.Fatalf("a write outside the workspace counted: %v %v", paths, all)
	}
	if err := os.WriteFile(filepath.Join(home, "y"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "d", "z"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(home, "y"), filepath.Join(home, "d", "y2")); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"y": false, "d/z": false, "d/y2": false}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		paths, all := w.Take()
		if all {
			t.Fatal("named changes reported as everything")
		}
		for _, p := range paths {
			if _, ok := want[p]; ok {
				want[p] = true
			}
		}
		done := true
		for _, seen := range want {
			done = done && seen
		}
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("changes not all named: %v", want)
}
