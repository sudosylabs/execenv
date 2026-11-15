package watch

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func event(mask uint64, info []byte) []byte {
	b := make([]byte, unix.FAN_EVENT_METADATA_LEN+len(info))
	binary.NativeEndian.PutUint32(b[0:4], uint32(len(b)))
	b[4] = 3 // version
	binary.NativeEndian.PutUint16(b[6:8], unix.FAN_EVENT_METADATA_LEN)
	binary.NativeEndian.PutUint64(b[8:16], mask)
	copy(b[unix.FAN_EVENT_METADATA_LEN:], info)
	return b
}

// An overflow, or a record the watcher cannot place, counts as a change.
func TestParseErrsTowardsWalking(t *testing.T) {
	w := &Watcher{known: map[string]bool{}}
	w.parse(event(unix.FAN_Q_OVERFLOW, nil))
	if !w.Changed() {
		t.Fatal("overflow not reported")
	}
	w.parse(event(unix.FAN_MODIFY, []byte{unix.FAN_EVENT_INFO_TYPE_DFID_NAME, 0, 2, 0}))
	if !w.Changed() {
		t.Fatal("unplaceable record not reported")
	}
	w.parse([]byte{1, 2, 3})
	if w.Changed() {
		t.Fatal("a short read invented a change")
	}
}

// Live: needs root and fanotify. A write in the workspace is seen; one
// outside it on the same filesystem is not.
func TestWatcherSeesWorkspaceWrites(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("fanotify filesystem marks need root")
	}
	base := t.TempDir()
	home := filepath.Join(base, "workspace")
	other := filepath.Join(base, "other")
	for _, d := range []string{home, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w, err := New(home)
	if err != nil {
		t.Skipf("fanotify unavailable: %v", err)
	}
	defer w.Close()
	w.Changed()
	if err := os.WriteFile(filepath.Join(other, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if w.Changed() {
		t.Fatal("a write outside the workspace counted")
	}
	if err := os.WriteFile(filepath.Join(home, "y"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.Changed() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a write in the workspace was not seen")
}
