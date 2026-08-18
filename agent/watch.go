package agent

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/sudosylabs/execenv"
)

const (
	watchBuffer = 32
	pollEvery   = 50 * time.Millisecond
)

type fileMeta struct {
	dir  bool
	hash [sha256.Size]byte
}

func (s *server) resetSnap() {
	s.snap = scanHome(s.home)
}

func (s *server) stopPoll(err error) {
	if !s.watching {
		return
	}
	s.watching = false
	s.watchErr = err
	if s.pollStop != nil {
		close(s.pollStop)
		s.pollStop = nil
	}
	_ = s.sess.send(frame{
		Kind:   kindWatch,
		Status: statusOf(err),
	})
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
	if !s.watching {
		return
	}
	next := scanHome(s.home)
	events := diffSnap(s.snap, next)
	if len(events) > watchBuffer {
		s.stopPoll(execenv.ErrLagged)
		return
	}
	for _, ev := range events {
		raw, err := encodeExtra(ev)
		if err != nil {
			s.stopPoll(execenv.ErrUnavailable)
			return
		}
		if err := s.sess.send(frame{Kind: kindWatch, Extra: raw}); err != nil {
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
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			out[rel] = fileMeta{dir: true}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		sum, err := hashFile(path)
		if err != nil {
			return nil
		}
		out[rel] = fileMeta{hash: sum}
		return nil
	})
	return out
}

func hashFile(path string) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, err
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
