// Package content keeps file bodies on disk, addressed by SHA-256.
//
// A body arrives as a stream and is hashed while it is written, so no body
// is ever held in memory and none is limited by it. Each body is stored
// once, under its digest; what nothing references any more is swept. A
// full disk is execenv.ErrQuota.
package content

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sudosylabs/execenv"
)

// Blob names one body: its digest and size. Two blobs with the same digest
// are the same bytes.
type Blob struct {
	Digest string
	Size   int64
}

// Store is a directory of bodies. It is safe for concurrent use: writers
// stage under unique names and publish by rename.
type Store struct{ dir string }

// stagePrefix marks bodies still being written; a sweep removes stale ones.
const stagePrefix = ".stage-"

// Open returns the store in dir, creating it.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir is where the store keeps its bodies.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(b Blob) string { return filepath.Join(s.dir, b.Digest) }

// Put stores r's bytes.
func (s *Store) Put(r io.Reader) (Blob, error) {
	stage, err := os.CreateTemp(s.dir, stagePrefix+"*")
	if err != nil {
		return Blob{}, diskError(err)
	}
	name := stage.Name()
	sum := sha256.New()
	size, err := io.Copy(io.MultiWriter(stage, sum), r)
	if cerr := stage.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return Blob{}, diskError(err)
	}
	b := Blob{Digest: hex.EncodeToString(sum.Sum(nil)), Size: size}
	if err := os.Rename(name, s.path(b)); err != nil {
		_ = os.Remove(name)
		return Blob{}, diskError(err)
	}
	return b, nil
}

// Has reports whether b is stored.
func (s *Store) Has(b Blob) bool {
	info, err := os.Stat(s.path(b))
	return err == nil && info.Size() == b.Size
}

// Open opens b for reading.
func (s *Store) Open(b Blob) (*os.File, error) {
	f, err := os.Open(s.path(b))
	if err != nil {
		return nil, execenv.ErrConflict
	}
	return f, nil
}

// Place writes b at dest with mode, replacing what is there. It copies, so
// later writes to dest never change the stored body.
func (s *Store) Place(b Blob, dest string, mode os.FileMode) error {
	src, err := s.Open(b)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".execenv-"+rand.Text())
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return diskError(err)
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return diskError(err)
	}
	return nil
}

// sweepGrace protects a body just written and not yet referenced, and a
// stage still being written, from a concurrent sweep.
const sweepGrace = 2 * time.Minute

// Sweep removes every body live does not name, and every abandoned stage,
// once they are older than the grace.
func (s *Store) Sweep(live map[string]bool) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-sweepGrace)
	for _, entry := range entries {
		name := entry.Name()
		if live[name] && !strings.HasPrefix(name, stagePrefix) {
			continue
		}
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
	return nil
}

// Remove deletes b now. Callers use it only for a body they know nothing
// else references.
func (s *Store) Remove(b Blob) { _ = os.Remove(s.path(b)) }

// Hash returns r's digest and size without storing it.
func Hash(r io.Reader) (Blob, error) {
	sum := sha256.New()
	size, err := io.Copy(sum, r)
	if err != nil {
		return Blob{}, err
	}
	return Blob{Digest: hex.EncodeToString(sum.Sum(nil)), Size: size}, nil
}

// diskError maps a full disk or quota to ErrQuota.
func diskError(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return execenv.ErrQuota
	}
	return err
}

// DiskError is diskError for callers writing the workspace themselves.
func DiskError(err error) error { return diskError(err) }
