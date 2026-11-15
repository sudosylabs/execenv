// Package watch tells the journal which paths of a workspace changed, so it
// reads those again and never walks an idle workspace.
//
// On Linux it places one fanotify mark on the workspace's whole filesystem
// (FAN_MARK_FILESYSTEM, reporting directory handles and names), which sees
// every create, write, move and delete at any depth without one watch per
// directory and without the race a new subdirectory gives inotify. Each
// event names an entry by its directory and name; events from outside the
// workspace on the same disk are left out. A queue overflow, or an event it
// cannot place, means everything may have changed: the watcher errs towards
// walking, never towards missing a change.
//
// Every event also names the process that caused it. Given a way to map a
// process to the tag of the started process it descends from, the watcher
// keeps, for each changed path, the tag of its last writer.
package watch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const events = unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_DELETE_SELF | unix.FAN_MOVED_FROM |
	unix.FAN_MOVED_TO | unix.FAN_MODIFY | unix.FAN_CLOSE_WRITE | unix.FAN_ATTRIB | unix.FAN_ONDIR

// maxDirty is how many changed paths the watcher keeps before it gives up
// naming them and reports that everything may have changed.
const maxDirty = 100_000

// Watcher watches one workspace.
type Watcher struct {
	home string
	// queue is the fanotify descriptor, non-blocking. Take reads it dry:
	// the kernel queues an event within the syscall that caused it, so
	// every change finished before Take is in what Take returns.
	queue int
	mount int
	buf   []byte

	mu    sync.Mutex
	dirty map[string]struct{}
	all   bool
	// writer maps a process id to the tag its process descends from, ""
	// for none; writers holds that tag for each changed path's last writer.
	writer  func(pid int) string
	writers map[string]string
}

// New watches home. It needs CAP_SYS_ADMIN and Linux 5.9 or later; without
// them it fails and the journal walks on every Observe instead. writer, when
// set, maps the process behind each event to the tag Writers reports.
func New(home string, writer func(pid int) string) (*Watcher, error) {
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
	w := &Watcher{home: real, queue: fd, mount: mount, buf: make([]byte, 64<<10), dirty: map[string]struct{}{}, all: true, writer: writer}
	return w, nil
}

// Take returns, and forgets, the paths under the workspace that changed
// since the last call, relative to it; all is set when the watcher cannot
// say which, and then paths is empty.
func (w *Watcher) Take() (paths []string, all bool) {
	w.drain()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.all {
		w.all = false
		w.dirty = map[string]struct{}{}
		return nil, true
	}
	paths = make([]string, 0, len(w.dirty))
	for p := range w.dirty {
		paths = append(paths, p)
	}
	w.dirty = map[string]struct{}{}
	return paths, false
}

// Writers returns, and forgets, the tag of each changed path's last writer
// that the events Take has read so far named, where the writer mapped one.
func (w *Watcher) Writers() map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	writers := w.writers
	w.writers = nil
	return writers
}

// everything marks that the watcher cannot name what changed.
func (w *Watcher) everything() {
	w.mu.Lock()
	w.all = true
	w.dirty = map[string]struct{}{}
	w.mu.Unlock()
}

func (w *Watcher) mark(rel string, pid int) {
	tag := ""
	if w.writer != nil && pid > 0 {
		tag = w.writer(pid)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// The last writer wins, and an unknown one clears an earlier tag.
	if tag != "" || w.writers[rel] != "" {
		if w.writers == nil {
			w.writers = make(map[string]string)
		}
		if len(w.writers) >= maxDirty {
			w.writers = make(map[string]string)
		}
		w.writers[rel] = tag
	}
	if w.all {
		return
	}
	w.dirty[rel] = struct{}{}
	if len(w.dirty) > maxDirty {
		w.all = true
		w.dirty = map[string]struct{}{}
	}
}

// Close stops watching.
func (w *Watcher) Close() error {
	err := unix.Close(w.queue)
	_ = unix.Close(w.mount)
	return err
}

// drain reads every queued event. A watcher with no queue (-1) has none.
func (w *Watcher) drain() {
	if w.queue < 0 {
		return
	}
	for {
		n, err := unix.Read(w.queue, w.buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			return
		}
		if err != nil || n <= 0 {
			// A broken queue cannot vouch for anything.
			w.everything()
			return
		}
		w.parse(w.buf[:n])
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
			w.everything()
			return
		}
		if mask&unix.FAN_Q_OVERFLOW != 0 {
			w.everything()
		} else {
			// The metadata's pid is the thread group of the process that
			// caused the event.
			pid := int(int32(binary.NativeEndian.Uint32(b[20:24])))
			w.place(b[metaLen:length], pid)
		}
		b = b[length:]
	}
}

// place marks the path an event's directory-and-name record names. An
// event it cannot place marks everything.
func (w *Watcher) place(info []byte, pid int) {
	for len(info) >= 4 {
		kind := info[0]
		size := int(binary.NativeEndian.Uint16(info[2:4]))
		if size < 4 || size > len(info) {
			w.everything()
			return
		}
		if kind == unix.FAN_EVENT_INFO_TYPE_DFID_NAME || kind == unix.FAN_EVENT_INFO_TYPE_DFID || kind == unix.FAN_EVENT_INFO_TYPE_FID {
			rel, inside, ok := w.resolve(info[4:size], kind == unix.FAN_EVENT_INFO_TYPE_DFID_NAME)
			switch {
			case !ok:
				w.everything()
			case inside:
				w.mark(rel, pid)
			}
			return
		}
		info = info[size:]
	}
	w.everything()
}

// resolve turns a record into a path relative to the workspace: its
// directory's handle, then for a named record the entry's name. ok is
// false when the handle no longer opens.
func (w *Watcher) resolve(record []byte, named bool) (rel string, inside, ok bool) {
	// __kernel_fsid_t (8 bytes), then struct file_handle, then the name.
	if len(record) < 16 {
		return "", false, false
	}
	bytes := int(binary.NativeEndian.Uint32(record[8:12]))
	kind := int32(binary.NativeEndian.Uint32(record[12:16]))
	if bytes < 0 || len(record) < 16+bytes {
		return "", false, false
	}
	handle := record[16 : 16+bytes]
	fd, err := unix.OpenByHandleAt(w.mount, unix.NewFileHandle(kind, handle), unix.O_PATH|unix.O_CLOEXEC)
	if err != nil {
		return "", false, false
	}
	dir, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	_ = unix.Close(fd)
	if err != nil {
		return "", false, false
	}
	var base string
	switch {
	case dir == w.home:
		base = "."
	case strings.HasPrefix(dir, w.home+"/"):
		base = strings.TrimPrefix(dir, w.home+"/")
	default:
		return "", false, true
	}
	if !named {
		return base, true, true
	}
	name := record[16+bytes:]
	if i := strings.IndexByte(string(name), 0); i >= 0 {
		name = name[:i]
	}
	if len(name) == 0 || string(name) == "." {
		return base, true, true
	}
	return path.Join(base, string(name)), true, true
}

func realPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
