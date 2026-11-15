// Package seed keeps a host's seeds: prepared trees, unpacked once from an
// archive and verified against their declared digest.
//
// Archives are untrusted. Unpacking accepts only directories and regular
// files, opens every path beneath the seed's own root so nothing can land
// outside it, and refuses a tree whose digest differs from the one declared
// before anything is kept.
package seed

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/archive"
	"github.com/sudosylabs/execenv/internal/content"
)

// Store is a directory of seeds, one subdirectory per digest. Each holds
// the tree under Tree and its record in seed.json; an adapter may keep more
// beside them (an image built from the tree).
type Store struct {
	dir string
	// Prepare, when set, runs on a freshly unpacked seed's directory before
	// it is published, to build whatever the adapter needs from the tree.
	Prepare func(dir string) error

	mu sync.Mutex
}

const (
	treeName   = "tree"
	recordName = "seed.json"
	stagePref  = ".stage-"
)

// Open returns the store in dir, creating it and clearing unfinished stages.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagePref) {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return &Store{dir: dir}, nil
}

// Dir is where seed digest is kept, existing or not.
func (s *Store) Dir(digest string) string { return filepath.Join(s.dir, digest) }

// Tree is the unpacked tree of seed digest.
func (s *Store) Tree(digest string) string { return filepath.Join(s.Dir(digest), treeName) }

// Get returns a seed the store holds, or ErrNotFound.
func (s *Store) Get(digest string) (execenv.Seed, error) {
	if execenv.ValidateSeedDigest(digest) != nil {
		return execenv.Seed{}, execenv.ErrInvalid
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(digest), recordName))
	if err != nil {
		return execenv.Seed{}, execenv.ErrNotFound
	}
	var out execenv.Seed
	if json.Unmarshal(raw, &out) != nil || out.Digest != digest {
		return execenv.Seed{}, execenv.ErrNotFound
	}
	return out, nil
}

// Put unpacks archive as seed digest. A seed already held is returned at
// once, without reading the archive.
func (s *Store) Put(digest string, r io.Reader) (execenv.Seed, error) {
	if execenv.ValidateSeedDigest(digest) != nil || r == nil {
		return execenv.Seed{}, execenv.ErrInvalid
	}
	if held, err := s.Get(digest); err == nil {
		return held, nil
	}
	stage, err := os.MkdirTemp(s.dir, stagePref)
	if err != nil {
		return execenv.Seed{}, content.DiskError(err)
	}
	defer os.RemoveAll(stage)
	tree := filepath.Join(stage, treeName)
	if err := os.Mkdir(tree, 0o755); err != nil {
		return execenv.Seed{}, content.DiskError(err)
	}
	root, err := os.OpenRoot(tree)
	if err != nil {
		return execenv.Seed{}, err
	}
	defer root.Close()
	record := execenv.Seed{Digest: digest}
	got, err := archive.Read(r, func(e archive.Entry, body io.Reader) error {
		record.Entries++
		if e.Dir {
			return content.DiskError(root.Mkdir(e.Path, 0o755))
		}
		mode := os.FileMode(0o644)
		if e.Executable {
			mode = 0o755
		}
		f, err := root.OpenFile(e.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return content.DiskError(err)
		}
		n, err := io.Copy(f, body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		record.Bytes += n
		return content.DiskError(err)
	})
	if err != nil {
		if errors.Is(err, execenv.ErrQuota) {
			return execenv.Seed{}, err
		}
		return execenv.Seed{}, execenv.ErrInvalid
	}
	if got != digest {
		return execenv.Seed{}, execenv.ErrInvalid
	}
	if s.Prepare != nil {
		if err := s.Prepare(stage); err != nil {
			return execenv.Seed{}, err
		}
	}
	raw, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(stage, recordName), raw, 0o600); err != nil {
		return execenv.Seed{}, content.DiskError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if held, err := s.Get(digest); err == nil {
		return held, nil
	}
	if err := os.Rename(stage, s.Dir(digest)); err != nil {
		return execenv.Seed{}, err
	}
	return record, nil
}

// List returns every seed held, by digest.
func (s *Store) List() ([]execenv.Seed, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []execenv.Seed{}
	for _, e := range entries {
		if held, err := s.Get(e.Name()); err == nil {
			out = append(out, held)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

// Delete removes seed digest. Removing one not held is a no-op.
func (s *Store) Delete(digest string) error {
	if execenv.ValidateSeedDigest(digest) != nil {
		return execenv.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(s.Dir(digest))
}
