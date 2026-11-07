//go:build linux

package agent

import (
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// degradedPoll is the rescan interval when the kernel refuses inotify (for
// example an exhausted watch limit). It is slow on purpose; the guest still
// converges, only with more latency.
const degradedPoll = time.Second

const inotifyMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MODIFY | unix.IN_CLOSE_WRITE |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ATTRIB | unix.IN_DELETE_SELF |
	unix.IN_MOVE_SELF | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW | unix.IN_EXCL_UNLINK

func newNotifier() notifier {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return newTickNotifier(degradedPoll)
	}
	n := &inotifyNotifier{file: os.NewFile(uintptr(fd), "inotify"), fd: fd, ch: make(chan struct{}, 1)}
	go n.read()
	return n
}

// inotifyNotifier wakes the watcher on kernel filesystem notifications. A
// registration failure switches it to a slow tick instead of going quiet.
type inotifyNotifier struct {
	file *os.File
	fd   int
	ch   chan struct{}

	mu       sync.Mutex
	closed   bool
	fallback *tickNotifier
}

func (n *inotifyNotifier) Events() <-chan struct{} { return n.ch }

func (n *inotifyNotifier) Add(dir string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.fallback != nil {
		return
	}
	if _, err := unix.InotifyAddWatch(n.fd, dir, inotifyMask); err != nil {
		// ENOENT means the directory vanished mid-scan; its parent's watch
		// already reported that. Anything else leaves a blind spot.
		if err == unix.ENOENT {
			return
		}
		n.fallback = newTickNotifier(degradedPoll)
		go n.forward(n.fallback)
	}
}

func (n *inotifyNotifier) forward(t *tickNotifier) {
	for {
		select {
		case <-t.stop:
			return
		case <-t.Events():
			n.signal()
		}
	}
}

func (n *inotifyNotifier) signal() {
	select {
	case n.ch <- struct{}{}:
	default:
	}
}

// read drains the queue. Event bodies are ignored: any record, including a
// queue overflow, only means "rescan".
func (n *inotifyNotifier) read() {
	buf := make([]byte, 64*1024)
	for {
		count, err := n.file.Read(buf)
		if count > 0 {
			n.signal()
		}
		if err != nil {
			return
		}
	}
}

func (n *inotifyNotifier) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	n.closed = true
	if n.fallback != nil {
		n.fallback.Close()
	}
	_ = n.file.Close()
}
