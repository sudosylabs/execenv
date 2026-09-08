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

// Capture neither closes nor mutates previous. Keep previous's descriptors
// open until this call succeeds and the observation transaction is retained.
// Failure is a typed resynchronization error, never a plausible empty tree.
func Capture(home string, previous *Snapshot) (result *Snapshot, err error) {
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
		entries, err := parent.ReadDir(execenv.MaxTreeEntries + 1)
		_ = parent.Close()
		if err != nil && err != io.EOF {
			return execenv.ErrLagged
		}
		if len(entries) > execenv.MaxTreeEntries {
			return execenv.ErrLagged
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
