// Package guesttree captures a bounded guest tree while its owner holds the
// workload write barrier. Open descriptors pin physical identities between
// captures; content hashes are never used to infer identity or directory moves.
package guesttree

import (
	"crypto/rand"
	"io"
	"os"
	"path"

	"github.com/sudosylabs/execenv"
)

type Node struct {
	Identity string
	Kind     execenv.ProjectionKind
	Data     []byte
	file     *os.File
	key      physicalKey
}

type Snapshot struct{ Nodes map[string]Node }

func (s *Snapshot) Close() {
	if s == nil {
		return
	}
	for _, node := range s.Nodes {
		if node.file != nil {
			_ = node.file.Close()
		}
	}
}

// Observe decides whether a directory entry enters the capture. held is true
// when previous already captured that path or, after a move, its physical
// node. A directory left out is never descended into. A nil Observe admits
// everything.
type Observe func(name string, dir, held bool) bool

// admitted reads every entry of one directory and keeps only those observe
// admits. A left-out entry costs its name, plus one Lstat when its path is
// new, and it can never lag the capture: it may be a link, a hard-linked
// file or a vanished path, and none of that is reported.
func admitted(root *os.Root, parent *os.File, directory string, previous *Snapshot, identities map[physicalKey]string, observe Observe) ([]os.DirEntry, error) {
	var entries []os.DirEntry
	for {
		batch, err := parent.ReadDir(256)
		for _, entry := range batch {
			if observe != nil {
				name := path.Join(directory, entry.Name())
				_, held := previous.node(name)
				if !observe(name, entry.IsDir(), held) && !moved(root, name, identities, observe) {
					continue
				}
			}
			entries = append(entries, entry)
			if len(entries) > execenv.MaxTreeEntries {
				return nil, execenv.ErrLagged
			}
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
func moved(root *os.Root, name string, identities map[physicalKey]string, observe Observe) bool {
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
		return false
	}
	key, err := physicalIdentity(info)
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
	var total int64
	var visit func(string) error
	visit = func(directory string) error {
		parent, err := root.Open(directory)
		if err != nil {
			return execenv.ErrLagged
		}
		entries, err := admitted(root, parent, directory, previous, identities, observe)
		_ = parent.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := path.Join(directory, entry.Name())
			if execenv.ValidatePath(name) != nil || len(result.Nodes) >= execenv.MaxTreeEntries {
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
			key, err := physicalIdentity(info)
			if err != nil || seen[key] {
				return execenv.ErrLagged
			}
			file, err := root.Open(name)
			if err != nil {
				return execenv.ErrLagged
			}
			actual, err := file.Stat()
			if err != nil || !os.SameFile(info, actual) {
				_ = file.Close()
				return execenv.ErrLagged
			}
			identity := identities[key]
			if identity == "" {
				identity = rand.Text()
			}
			node := Node{Identity: identity, Kind: kind, file: file, key: key}
			result.Nodes[name] = node
			seen[key] = true
			if kind == execenv.ProjectionFile {
				data, err := io.ReadAll(io.LimitReader(file, execenv.MaxFileBytes+1))
				if err != nil || len(data) > execenv.MaxFileBytes {
					return execenv.ErrLagged
				}
				final, err := file.Stat()
				if err != nil || final.Size() != int64(len(data)) || !final.ModTime().Equal(info.ModTime()) {
					return execenv.ErrLagged
				}
				total += int64(len(data))
				if total > execenv.MaxTreeBytes {
					return execenv.ErrLagged
				}
				node.Data = data
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
