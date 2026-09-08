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
	pollEvery   = 50 * time.Millisecond
)

type fileMeta struct {
	unsupported bool
	dir         bool
	hash        [sha256.Size]byte
}

func (s *server) resetSnap() {
	s.snap = scanHome(s.home)
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

func (s *server) poll(stop <-chan struct{}) {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
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
	next := scanHome(s.home)
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

func scanHome(home string) map[string]fileMeta {
	out := make(map[string]fileMeta)
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			out[path] = fileMeta{unsupported: true}
			return nil
		}
		if path == home {
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
			out[rel] = fileMeta{dir: true}
			return nil
		}
		if !d.Type().IsRegular() {
			out[rel] = fileMeta{unsupported: true}
			return nil
		}
		sum, err := hashFile(path)
		if err != nil {
			out[rel] = fileMeta{unsupported: true}
			return nil
		}
		out[rel] = fileMeta{hash: sum}
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
