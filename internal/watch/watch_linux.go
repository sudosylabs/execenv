// Package watch tells the journal when a workspace may have changed, so an
// idle workspace is never walked.
//
// On Linux it places one fanotify mark on the workspace's whole filesystem
// (FAN_MARK_FILESYSTEM, reporting directory handles and names), which sees
// every create, write, move and delete at any depth without one watch per
// directory and without the race a new subdirectory gives inotify. Events
// from outside the workspace on the same disk are left out. A queue
// overflow, or an event it cannot place, counts as a change: the watcher
// errs towards walking, never towards missing one.
package watch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const events = unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_DELETE_SELF | unix.FAN_MOVED_FROM |
	unix.FAN_MOVED_TO | unix.FAN_MODIFY | unix.FAN_CLOSE_WRITE | unix.FAN_ATTRIB | unix.FAN_ONDIR

// Watcher watches one workspace.
type Watcher struct {
	home    string
	queue   *os.File
	mount   int
	changed atomic.Bool

	mu    sync.Mutex
	known map[string]bool
}

// New watches home. It needs CAP_SYS_ADMIN and Linux 5.9 or later; without
// them it fails and the journal walks on every Observe instead.
func New(home string) (*Watcher, error) {
	real, err := realPath(home)
	if err != nil {
		return nil, err
	}
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK|unix.FAN_REPORT_DFID_NAME, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("fanotify: %w", err)
	}
	if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM, events, unix.AT_FDCWD, real); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("fanotify mark: %w", err)
	}
	mount, err := unix.Open(real, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	w := &Watcher{home: real, queue: os.NewFile(uintptr(fd), "fanotify"), mount: mount, known: make(map[string]bool)}
	w.changed.Store(true)
	go w.read()
	return w, nil
}

// Changed reports, and forgets, whether anything under the workspace may
// have changed since the last call.
func (w *Watcher) Changed() bool { return w.changed.Swap(false) }

// Close stops watching.
func (w *Watcher) Close() error {
	err := w.queue.Close()
	_ = unix.Close(w.mount)
	return err
}

func (w *Watcher) read() {
	buf := make([]byte, 64<<10)
	for {
		n, err := w.queue.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			// A broken queue cannot vouch for anything.
			w.changed.Store(true)
			return
		}
		w.parse(buf[:n])
	}
}

// parse walks a batch of events. Each is fanotify_event_metadata followed
// by info records; a DFID_NAME record holds the fsid, the directory's file
// handle and the entry's name.
func (w *Watcher) parse(b []byte) {
	for len(b) >= unix.FAN_EVENT_METADATA_LEN {
		length := int(binary.NativeEndian.Uint32(b[0:4]))
		metaLen := int(binary.NativeEndian.Uint16(b[6:8]))
		mask := binary.NativeEndian.Uint64(b[8:16])
		if length < metaLen || length > len(b) {
			w.changed.Store(true)
			return
		}
		if mask&unix.FAN_Q_OVERFLOW != 0 || w.relevant(b[metaLen:length]) {
			w.changed.Store(true)
		}
		b = b[length:]
	}
}

// relevant reports whether an event's directory lies in the workspace.
func (w *Watcher) relevant(info []byte) bool {
	for len(info) >= 4 {
		kind := info[0]
		size := int(binary.NativeEndian.Uint16(info[2:4]))
		if size < 4 || size > len(info) {
			return true
		}
		if kind == unix.FAN_EVENT_INFO_TYPE_DFID_NAME || kind == unix.FAN_EVENT_INFO_TYPE_DFID || kind == unix.FAN_EVENT_INFO_TYPE_FID {
			return w.inside(info[4:size])
		}
		info = info[size:]
	}
	return true
}

// inside resolves the record's directory handle and checks it is under the
// workspace. Anything it cannot resolve counts as inside.
func (w *Watcher) inside(record []byte) bool {
	// __kernel_fsid_t (8 bytes), then struct file_handle.
	if len(record) < 16 {
		return true
	}
	bytes := int(binary.NativeEndian.Uint32(record[8:12]))
	kind := int32(binary.NativeEndian.Uint32(record[12:16]))
	if len(record) < 16+bytes {
		return true
	}
	handle := record[16 : 16+bytes]
	key := fmt.Sprintf("%d:%x", kind, handle)
	w.mu.Lock()
	known, ok := w.known[key]
	w.mu.Unlock()
	if ok {
		return known
	}
	fd, err := unix.OpenByHandleAt(w.mount, unix.NewFileHandle(kind, handle), unix.O_PATH|unix.O_CLOEXEC)
	if err != nil {
		return true
	}
	path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	_ = unix.Close(fd)
	if err != nil {
		return true
	}
	inside := path == w.home || strings.HasPrefix(path, w.home+"/")
	w.mu.Lock()
	if len(w.known) > 4096 {
		w.known = make(map[string]bool)
	}
	w.known[key] = inside
	w.mu.Unlock()
	return inside
}

func realPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
