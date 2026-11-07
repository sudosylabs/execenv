package agent

import (
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/watchlog"
)

const (
	watchBuffer = watchlog.DefaultCapacity
	// settleDelay is the quiet period after a filesystem notification
	// before the tree is rescanned. Bursts of writes coalesce into one scan.
	settleDelay = 25 * time.Millisecond
	// maxSettle bounds how long a continuous burst can postpone a scan.
	maxSettle = 250 * time.Millisecond
	// safetyRescan catches anything notification missed (an unwatchable
	// directory, a dropped queue, a filesystem without notifications).
	safetyRescan = 10 * time.Second
	// racyWindow treats files whose change time is this close to the scan
	// as unverified, so a same-tick rewrite with equal size and timestamps
	// is still hashed on the next scan.
	racyWindow = 100 * time.Millisecond
)

// fileMeta is one observed path. stat is the cheap identity compared before
// any file body is read; a zero stat means the hash must be recomputed.
type fileMeta struct {
	unsupported bool
	dir         bool
	hash        [sha256.Size]byte
	stat        statKey
}

// statKey is the metadata a rescan compares before hashing. Equal keys with
// trusted set reuse the previous hash; anything else is hashed again.
type statKey struct {
	trusted bool
	dev     uint64
	ino     uint64
	size    int64
	mtime   int64
	ctime   int64
}

func (s *server) resetSnap() {
	s.snap = scanHome(s.home, nil, s.watchDir)
}

func (s *server) watchDir(path string) {
	if s.notify != nil {
		s.notify.Add(path)
	}
}

// preparePoll registers notifications and takes the baseline. Directories
// are registered before the scan reads them, so no change between the scan
// and the first notification is lost.
func (s *server) preparePoll() {
	if s.pollStop != nil || s.notify != nil {
		return
	}
	s.notify = newNotifier()
	s.resetSnap()
}

// discardPoll releases a prepared baseline that was never started.
func (s *server) discardPoll() {
	if s.pollStop == nil && s.notify != nil {
		s.notify.Close()
		s.notify = nil
	}
}

// startPoll runs the watcher for a prepared baseline.
func (s *server) startPoll() {
	if s.pollStop != nil {
		return
	}
	if s.notify == nil {
		s.preparePoll()
	}
	s.kick = make(chan struct{}, 1)
	s.pollStop = make(chan struct{})
	go s.poll(s.pollStop, s.notify, s.kick)
}

// kickPoll asks the watcher for a prompt rescan, for example after thaw.
func (s *server) kickPoll() {
	if s.kick == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *server) stopPoll(err error) {
	wasWatching := s.watching
	stream := s.watchStream
	s.watching = false
	s.watchStream = 0
	s.watchErr = err
	if s.pollStop != nil {
		close(s.pollStop)
		s.pollStop = nil
	}
	if s.notify != nil {
		s.notify.Close()
		s.notify = nil
	}
	s.kick = nil
	if wasWatching {
		_ = s.sess.Send(frame{
			Kind:   kindWatch,
			Stream: stream,
			Status: statusOf(err),
		})
	}
}

func (s *server) invalidateWatch(err error) {
	if !s.watching {
		return
	}
	s.watching = false
	stream := s.watchStream
	s.watchStream = 0
	s.watchErr = err
	_ = s.sess.Send(frame{Kind: kindWatch, Stream: stream, Status: statusOf(err)})
}

// poll rescans after coalesced filesystem notifications, after a kick,
// and on a slow safety timer. The rescan is authoritative; notifications
// only decide when it runs, so a dropped or overflowed queue costs latency,
// never correctness.
func (s *server) poll(stop <-chan struct{}, n notifier, kick <-chan struct{}) {
	safety := time.NewTicker(safetyRescan)
	defer safety.Stop()
	settle := time.NewTimer(time.Hour)
	settle.Stop()
	defer settle.Stop()
	var burst time.Time
	pending := false
	for {
		select {
		case <-stop:
			return
		case <-n.Events():
			now := time.Now()
			if !pending {
				burst = now
				pending = true
			}
			delay := settleDelay
			if remaining := maxSettle - now.Sub(burst); remaining < delay {
				delay = max(remaining, 0)
			}
			settle.Reset(delay)
		case <-settle.C:
			pending = false
			s.diffOnce()
		case <-kick:
			s.diffOnce()
		case <-safety.C:
			s.diffOnce()
		}
	}
}

func (s *server) diffOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pollStop == nil || s.frozen {
		return
	}
	next := scanHome(s.home, s.snap, s.watchDir)
	if !validScan(s.snap) || !validScan(next) || changedKinds(s.snap, next) {
		s.invalidateWatch(execenv.ErrLagged)
		s.log.Reset()
		s.snap = next
		return
	}
	events := diffSnap(s.snap, next)
	if len(events) > watchBuffer {
		s.invalidateWatch(execenv.ErrLagged)
		s.log.Reset()
		s.snap = next
		return
	}
	for _, ev := range events {
		ev = s.log.Append(ev)
		if !s.watching {
			continue
		}
		raw, err := encodeExtra(ev)
		if err != nil {
			s.stopPoll(execenv.ErrUnavailable)
			return
		}
		if err := s.sess.Send(frame{Kind: kindWatch, Stream: s.watchStream, Extra: raw}); err != nil {
			s.stopPoll(execenv.ErrClosed)
			return
		}
	}
	s.snap = next
}

// scanHome walks home. A regular file whose stat key matches prev reuses
// its previous hash, so an idle tree costs metadata reads only. watch is
// called for every directory before its entries are read.
func scanHome(home string, prev map[string]fileMeta, watch func(string)) map[string]fileMeta {
	out := make(map[string]fileMeta, len(prev))
	racy := time.Now().Add(-racyWindow).UnixNano()
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			out[path] = fileMeta{unsupported: true}
			return nil
		}
		if path == home {
			if watch != nil {
				watch(path)
			}
			return nil
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if execenv.ValidatePath(rel) != nil {
			out[rel] = fileMeta{unsupported: true}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			out[rel] = fileMeta{unsupported: true}
			return nil
		}
		if d.IsDir() {
			if watch != nil {
				watch(path)
			}
			out[rel] = fileMeta{dir: true}
			return nil
		}
		if !d.Type().IsRegular() {
			out[rel] = fileMeta{unsupported: true}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			out[rel] = fileMeta{unsupported: true}
			return nil
		}
		key := statOf(info, racy)
		if old, ok := prev[rel]; ok && key.trusted && old.stat == key && !old.dir && !old.unsupported {
			out[rel] = old
			return nil
		}
		if info.Size() > execenv.MaxFileBytes {
			out[rel] = fileMeta{unsupported: true}
			return nil
		}
		sum, err := hashFile(path)
		if err != nil {
			out[rel] = fileMeta{unsupported: true}
			return nil
		}
		out[rel] = fileMeta{hash: sum, stat: key}
		return nil
	})
	return out
}

func hashFile(path string) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, execenv.MaxFileBytes+1))
	if err != nil {
		return zero, err
	}
	if len(data) > execenv.MaxFileBytes {
		return zero, execenv.ErrTooLarge
	}
	return sha256.Sum256(data), nil
}

func diffSnap(old, next map[string]fileMeta) []execenv.Event {
	var deleted, created []string
	var events []execenv.Event
	for path, meta := range old {
		cur, ok := next[path]
		if !ok {
			deleted = append(deleted, path)
			continue
		}
		if !meta.dir && !cur.dir && meta.hash != cur.hash {
			events = append(events, execenv.Event{Op: execenv.OpReplace, Path: path})
		}
	}
	for path := range next {
		if _, ok := old[path]; !ok {
			created = append(created, path)
		}
	}
	// One disappear and one appear with the same hash is a rename. Two
	// independent edits in the same poll stay create+delete.
	if len(deleted) == 1 && len(created) == 1 && !old[deleted[0]].dir && !next[created[0]].dir && old[deleted[0]].hash == next[created[0]].hash {
		return append(events, execenv.Event{Op: execenv.OpMove, Path: created[0], From: deleted[0]})
	}
	for _, path := range deleted {
		events = append(events, execenv.Event{Op: execenv.OpDelete, Path: path})
	}
	for _, path := range created {
		events = append(events, execenv.Event{Op: execenv.OpCreate, Path: path})
	}
	return events
}

// projectSnap advances only the exact paths affected by acknowledged caller
// mutations. Unrelated guest writes retain their previous observation baseline.
func (s *server) projectSnap(batch execenv.Batch) {
	if s.snap == nil {
		s.snap = make(map[string]fileMeta)
	}
	for _, mutation := range batch.Mutations {
		switch mutation.Op {
		case execenv.OpCreate, execenv.OpReplace:
			s.snap[mutation.Path] = fileMeta{dir: mutation.Kind == execenv.KindDirectory, hash: sha256.Sum256(mutation.Data)}
		case execenv.OpDelete:
			for key := range s.snap {
				if key == mutation.Path || strings.HasPrefix(key, mutation.Path+"/") {
					delete(s.snap, key)
				}
			}
		case execenv.OpMove:
			moved := make(map[string]fileMeta)
			for key, meta := range s.snap {
				if key == mutation.From || strings.HasPrefix(key, mutation.From+"/") {
					moved[mutation.Path+strings.TrimPrefix(key, mutation.From)] = meta
					delete(s.snap, key)
				}
			}
			for key, meta := range moved {
				s.snap[key] = meta
			}
		}
	}
}

func validScan(snapshot map[string]fileMeta) bool {
	for _, meta := range snapshot {
		if meta.unsupported {
			return false
		}
	}
	return true
}
func changedKinds(old, next map[string]fileMeta) bool {
	for path, meta := range old {
		if current, ok := next[path]; ok && current.dir != meta.dir {
			return true
		}
	}
	return false
}
