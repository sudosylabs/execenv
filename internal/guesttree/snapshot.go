// Package guesttree captures a guest tree. Physical identities come from the
// filesystem itself (on Linux, file handles carrying the inode generation),
// so nothing is held open between captures and a tree of any size costs no
// descriptors. Content hashes are never used to infer identity or moves. An
// unchanged file is neither opened nor hashed again.
package guesttree

import (
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
)

type Node struct {
	Identity string
	Kind     execenv.ProjectionKind
	// Content is a file's digest and size, never its bytes.
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

// Snapshot is one capture of the tree. The workload keeps running while it
// is taken, so it may miss what changed under it: Unsettled says so, and a
// capture soon after reports the rest.
type Snapshot struct {
	Nodes     map[string]Node
	Unsettled bool
}

// Close releases the snapshot. It holds nothing open; Close is kept so
// callers need not know that.
func (s *Snapshot) Close() {}

// Observe decides whether a directory entry enters the capture. held is true
// when previous already captured that path or, after a move, its physical
// node. A directory left out is never descended into. A nil Observe admits
// everything.
type Observe func(name string, dir, held bool) bool

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

// Capture walks the whole tree. It neither closes nor mutates previous.
// Failure is a typed resynchronization error, never a plausible empty tree.
// observe leaves ignored paths out; see Observe. Only new or changed files
// are hashed; nothing is copied.
func Capture(home string, previous *Snapshot, observe Observe) (*Snapshot, error) {
	c, err := newCapturer(home, previous, observe, map[string]Node{})
	if err != nil {
		return nil, err
	}
	defer c.root.Close()
	if err := c.visit("."); err != nil {
		return nil, err
	}
	return c.result, nil
}

// CaptureDirty is Capture for a tree that changed only at paths: it starts
// from previous and reads again just those paths and what is under them,
// so its cost is the change's, not the tree's. A path whose parent previous
// does not hold, or "." itself, is a whole walk.
func CaptureDirty(home string, previous *Snapshot, observe Observe, paths []string) (*Snapshot, error) {
	if previous == nil {
		return Capture(home, previous, observe)
	}
	roots, whole := dirtyRoots(paths)
	if whole {
		return Capture(home, previous, observe)
	}
	nodes := make(map[string]Node, len(previous.Nodes))
	for name, node := range previous.Nodes {
		if !underAny(name, roots) {
			nodes[name] = node
		}
	}
	for _, r := range roots {
		if parent := path.Dir(r); parent != "." {
			if node, ok := nodes[parent]; !ok || node.Kind != execenv.ProjectionDirectory {
				// A path under a directory it does not hold: the events
				// that would explain it are not here.
				return Capture(home, previous, observe)
			}
		}
	}
	c, err := newCapturer(home, previous, observe, nodes)
	if err != nil {
		return nil, err
	}
	defer c.root.Close()
	for _, r := range roots {
		info, err := c.root.Lstat(r)
		if errors.Is(err, fs.ErrNotExist) {
			continue // gone: previous's nodes under it are already dropped
		}
		if err != nil {
			return nil, execenv.ErrLagged
		}
		if c.observe != nil {
			_, held := previous.node(r)
			if !c.observe(r, info.IsDir(), held) && !moved(home, c.root, r, c.identities, c.observe) {
				continue
			}
		}
		if err := c.entry(r); err != nil {
			return nil, err
		}
	}
	return c.result, nil
}

// gone reports an error that means the path is not what was listed any
// more: removed, or a parent replaced by something that is not a
// directory.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// dirtyRoots is the smallest set of paths that covers every dirty one,
// sorted; whole is set when one of them is the tree itself.
func dirtyRoots(paths []string) (roots []string, whole bool) {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		p = path.Clean(p)
		if p == "." || p == "" || p == "/" {
			return nil, true
		}
		if execenv.ValidatePath(p) != nil {
			return nil, true
		}
		clean = append(clean, p)
	}
	slices.Sort(clean)
	for _, p := range slices.Compact(clean) {
		if !underAny(p, roots) {
			roots = append(roots, p)
		}
	}
	return roots, false
}

// underAny reports whether name is one of roots or below one.
func underAny(name string, roots []string) bool {
	for _, r := range roots {
		if name == r || strings.HasPrefix(name, r+"/") {
			return true
		}
	}
	return false
}

// capturer is one capture under way. result starts with the nodes it
// keeps from previous; visit and entry add the ones it reads.
type capturer struct {
	home          string
	root          *os.Root
	previous      *Snapshot
	observe       Observe
	identities    map[physicalKey]string
	previousByKey map[physicalKey]Node
	seen          map[physicalKey]string
	result        *Snapshot
}

func newCapturer(home string, previous *Snapshot, observe Observe, kept map[string]Node) (*capturer, error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, execenv.ErrLagged
	}
	c := &capturer{
		home: home, root: root, previous: previous, observe: observe,
		identities:    make(map[physicalKey]string),
		previousByKey: make(map[physicalKey]Node),
		seen:          make(map[physicalKey]string, len(kept)),
		result:        &Snapshot{Nodes: kept},
	}
	if previous != nil {
		for _, node := range previous.Nodes {
			c.identities[node.key] = node.Identity
			c.previousByKey[node.key] = node
		}
	}
	for name, node := range kept {
		c.seen[node.key] = name
	}
	return c, nil
}

// visit reads one directory's admitted entries.
func (c *capturer) visit(directory string) error {
	parent, err := c.root.Open(directory)
	if gone(err) {
		// Moved, removed or replaced since it was listed.
		c.result.Unsettled = true
		return nil
	}
	if err != nil {
		return execenv.ErrLagged
	}
	if st, err := parent.Stat(); err != nil || !st.IsDir() {
		_ = parent.Close()
		c.result.Unsettled = true
		return nil
	}
	entries, err := admitted(c.home, c.root, parent, directory, c.previous, c.identities, c.observe)
	_ = parent.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := c.entry(path.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// entry reads one admitted path: a file is hashed unless its stamp is as
// previous had it, a directory is visited.
func (c *capturer) entry(name string) error {
	if execenv.ValidatePath(name) != nil {
		return execenv.ErrLagged
	}
	info, err := c.root.Lstat(name)
	if gone(err) {
		c.result.Unsettled = true
		return nil
	}
	if err != nil {
		return execenv.ErrLagged
	}
	kind := execenv.ProjectionFile
	if info.IsDir() {
		kind = execenv.ProjectionDirectory
	} else if !info.Mode().IsRegular() {
		return execenv.ErrLagged
	}
	key, err := physicalIdentity(filepath.Join(c.home, filepath.FromSlash(name)), info)
	if err != nil {
		// A node it cannot name is unsupported (a hard link) unless it is
		// simply gone.
		if _, lerr := c.root.Lstat(name); !errors.Is(lerr, fs.ErrNotExist) {
			return execenv.ErrLagged
		}
	}
	if err != nil || c.seen[key] != "" {
		// Gone, or met again under another name: a move ran while the walk
		// did, and the next capture sees where it went. A hard link is not
		// this: physicalIdentity refuses a file with more than one link,
		// and a directory has only one name.
		c.result.Unsettled = true
		return nil
	}
	identity := c.identities[key]
	if identity == "" {
		identity = rand.Text()
	}
	node := Node{Identity: identity, Kind: kind, key: key}
	c.result.Nodes[name] = node
	c.seen[key] = name
	if kind == execenv.ProjectionDirectory {
		return c.visit(name)
	}
	st := stampOf(info)
	if old, ok := c.previousByKey[key]; ok && old.Kind == kind && old.stamp == st && old.Content.Digest != "" {
		node.Content, node.stamp = old.Content, st
		c.result.Nodes[name] = node
		return nil
	}
	blob, settled, err := hashFile(c.root, name, info, st)
	if errors.Is(err, fs.ErrNotExist) {
		delete(c.result.Nodes, name)
		delete(c.seen, key)
		c.result.Unsettled = true
		return nil
	}
	if err != nil {
		return err
	}
	// A file written while it was hashed keeps the stamp it was listed with,
	// so the next capture hashes it again.
	node.Content, node.stamp = blob, st
	c.result.Nodes[name] = node
	if !settled {
		c.result.Unsettled = true
	}
	return nil
}

// hashFile reads one file once to hash it. settled is false when the file
// changed while it was read: the hash is of the bytes read, which a writer
// may already have moved past. A file replaced since it was listed is
// fs.ErrNotExist: the listed one is gone.
func hashFile(root *os.Root, name string, listed os.FileInfo, st stamp) (blob content.Blob, settled bool, err error) {
	file, err := root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return content.Blob{}, false, fs.ErrNotExist
	}
	if err != nil {
		return content.Blob{}, false, execenv.ErrLagged
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(listed, actual) {
		return content.Blob{}, false, fs.ErrNotExist
	}
	blob, err = content.Hash(file)
	if err != nil {
		return content.Blob{}, false, execenv.ErrLagged
	}
	final, err := file.Stat()
	settled = err == nil && stampOf(actual) == st && final.Size() == blob.Size && stampOf(final) == st
	return blob, settled, nil
}
