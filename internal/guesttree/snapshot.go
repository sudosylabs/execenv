// Package guesttree captures a guest tree. Physical identities come from the
// filesystem itself (on Linux, file handles carrying the inode generation),
// so nothing is held open between captures and a tree of any size costs no
// descriptors. Content hashes are never used to infer identity or moves. An
// unchanged file is neither opened nor hashed again.
package guesttree

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
)

type Node struct {
	Identity string
	Kind     execenv.ProjectionKind
	// Content is a file's digest and size, never its bytes. The body is
	// copied into the content store only when a change reports it.
	Content content.Blob
	key     physicalKey
	// stamp is what a file's metadata was when Content was computed, so an
	// unchanged file is not hashed again.
	stamp stamp
}

// stamp is the metadata that changes whenever a file's bytes may have.
type stamp struct {
	size  int64
	mtime int64
	ctime int64
}

type Snapshot struct{ Nodes map[string]Node }

// Close releases the snapshot. It holds nothing open; Close is kept so
// callers need not know that.
func (s *Snapshot) Close() {}

// Observe decides whether a directory entry enters the capture. held is true
// when previous already captured that path or, after a move, its physical
// node. A directory left out is never descended into. A nil Observe admits
// everything.
type Observe func(name string, dir, held bool) bool

// ErrChanged is a capture that raced a writer: a file changed while it was
// read. It is ErrLagged, but a fresh capture may succeed.
var ErrChanged = fmt.Errorf("%w: changed while captured", execenv.ErrLagged)

// admitted reads every entry of one directory and keeps only those observe
// admits. A left-out entry costs its name, plus one Lstat when its path is
// new, and it can never lag the capture: it may be a link, a hard-linked
// file or a vanished path, and none of that is reported.
func admitted(home string, root *os.Root, parent *os.File, directory string, previous *Snapshot, identities map[physicalKey]string, observe Observe) ([]os.DirEntry, error) {
	var entries []os.DirEntry
	for {
		batch, err := parent.ReadDir(256)
		for _, entry := range batch {
			if observe != nil {
				name := path.Join(directory, entry.Name())
				_, held := previous.node(name)
				if !observe(name, entry.IsDir(), held) && !moved(home, root, name, identities, observe) {
					continue
				}
			}
			entries = append(entries, entry)
		}
		if err == io.EOF || err == nil && len(batch) == 0 {
			return entries, nil
		}
		if err != nil {
			return nil, execenv.ErrLagged
		}
	}
}

// moved reports whether a left-out path is a physical node previous already
// captured elsewhere, so a held entry stays observed across a move.
func moved(home string, root *os.Root, name string, identities map[physicalKey]string, observe Observe) bool {
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
		return false
	}
	key, err := physicalIdentity(filepath.Join(home, filepath.FromSlash(name)), info)
	if err != nil || identities[key] == "" {
		return false
	}
	return observe(name, info.IsDir(), true)
}

func (s *Snapshot) node(name string) (Node, bool) {
	if s == nil {
		return Node{}, false
	}
	node, ok := s.Nodes[name]
	return node, ok
}

// Capture neither closes nor mutates previous. Keep previous's descriptors
// open until this call succeeds and the observation transaction is retained.
// Failure is a typed resynchronization error, never a plausible empty tree.
// observe leaves ignored paths out; see Observe.
// Only new or changed files are hashed; nothing is copied.
func Capture(home string, previous *Snapshot, observe Observe) (result *Snapshot, err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, execenv.ErrLagged
	}
	defer root.Close()
	result = &Snapshot{Nodes: make(map[string]Node)}
	defer func() {
		if err != nil {
			result.Close()
			result = nil
		}
	}()
	identities := make(map[physicalKey]string)
	if previous != nil {
		for _, node := range previous.Nodes {
			identities[node.key] = node.Identity
		}
	}
	seen := make(map[physicalKey]bool)
	previousByKey := make(map[physicalKey]Node)
	if previous != nil {
		for _, node := range previous.Nodes {
			previousByKey[node.key] = node
		}
	}
	var visit func(string) error
	visit = func(directory string) error {
		parent, err := root.Open(directory)
		if err != nil {
			return execenv.ErrLagged
		}
		entries, err := admitted(home, root, parent, directory, previous, identities, observe)
		_ = parent.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := path.Join(directory, entry.Name())
			if execenv.ValidatePath(name) != nil {
				return execenv.ErrLagged
			}
			info, err := root.Lstat(name)
			if err != nil {
				return execenv.ErrLagged
			}
			kind := execenv.ProjectionFile
			if info.IsDir() {
				kind = execenv.ProjectionDirectory
			} else if !info.Mode().IsRegular() {
				return execenv.ErrLagged
			}
			key, err := physicalIdentity(filepath.Join(home, filepath.FromSlash(name)), info)
			if err != nil || seen[key] {
				return execenv.ErrLagged
			}
			identity := identities[key]
			if identity == "" {
				identity = rand.Text()
			}
			node := Node{Identity: identity, Kind: kind, key: key}
			result.Nodes[name] = node
			seen[key] = true
			if kind == execenv.ProjectionFile {
				st := stampOf(info)
				if old, ok := previousByKey[key]; ok && old.Kind == kind && old.stamp == st && old.Content.Digest != "" {
					node.Content, node.stamp = old.Content, st
					result.Nodes[name] = node
					continue
				}
				blob, err := hashFile(root, name, info, st)
				if err != nil {
					return err
				}
				node.Content, node.stamp = blob, st
				result.Nodes[name] = node
			} else if err := visit(name); err != nil {
				return err
			}
		}
		return nil
	}
	err = visit(".")
	return result, err
}

// hashFile reads one file once to hash it. A file that is not the one
// listed, or that changes while read, is ErrChanged.
func hashFile(root *os.Root, name string, listed os.FileInfo, st stamp) (content.Blob, error) {
	file, err := root.Open(name)
	if err != nil {
		return content.Blob{}, ErrChanged
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(listed, actual) || stampOf(actual) != st {
		return content.Blob{}, ErrChanged
	}
	blob, err := content.Hash(file)
	if err != nil {
		return content.Blob{}, execenv.ErrLagged
	}
	final, err := file.Stat()
	if err != nil || final.Size() != blob.Size || stampOf(final) != st {
		return content.Blob{}, ErrChanged
	}
	return blob, nil
}
