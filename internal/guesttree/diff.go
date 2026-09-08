package guesttree

import (
	"bytes"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/sudosylabs/execenv"
)

type Operation string

const (
	CreateFile      Operation = "create_file"
	CreateDirectory Operation = "create_directory"
	WriteFile       Operation = "write_file"
	Move            Operation = "move"
	Delete          Operation = "delete"
)

// Change has immutable capture-time content and an epoch-stable physical node
// identity. The owning observation log adds its durable projection baseline.
type Change struct {
	Operation   Operation
	Identity    string
	Kind        execenv.ProjectionKind
	Path        string
	Destination string
	Recursive   bool
	Data        []byte
}

// Diff constructs semantic operations only when stable captured identities
// prove a supported transformation. Cyclic moves, kind changes and identity
// gaps require resynchronization; none become guessed deletes or creates.
func Diff(before, after *Snapshot) ([]Change, error) {
	if before == nil || after == nil {
		return nil, execenv.ErrLagged
	}
	target := make(map[string]string, len(after.Nodes))
	for name, node := range after.Nodes {
		if node.Identity == "" || target[node.Identity] != "" {
			return nil, execenv.ErrLagged
		}
		target[node.Identity] = name
		if old, ok := before.Nodes[name]; ok && old.Kind != node.Kind {
			return nil, execenv.ErrLagged
		}
	}
	seen := make(map[string]bool)
	for _, node := range before.Nodes {
		if node.Identity == "" || seen[node.Identity] {
			return nil, execenv.ErrLagged
		}
		seen[node.Identity] = true
	}
	current := maps.Clone(before.Nodes)
	var changes []Change
	appendChange := func(op Operation, name, destination string, node Node, recursive bool) {
		changes = append(changes, Change{Operation: op, Identity: node.Identity, Kind: node.Kind, Path: name, Destination: destination, Recursive: recursive, Data: append([]byte(nil), node.Data...)})
	}
	parentReady := func(name string) bool {
		parent := path.Dir(name)
		if parent == "." {
			return true
		}
		actual, ok := current[parent]
		expected, exists := after.Nodes[parent]
		return ok && exists && actual.Kind == execenv.ProjectionDirectory && actual.Identity == expected.Identity
	}
	names := slices.Sorted(maps.Keys(after.Nodes))
	// Each pass makes progress on a finite identity/path set. Moves rewrite an
	// entire subtree, so descendants already at their destination emit no echo.
	for pass := 0; pass <= execenv.MaxTreeEntries*2; pass++ {
		progress := false
		for _, name := range slices.Sorted(maps.Keys(current)) {
			node, ok := current[name]
			if !ok || target[node.Identity] != "" {
				continue
			}
			removable := true
			for child, descendant := range current {
				if strings.HasPrefix(child, name+"/") && target[descendant.Identity] != "" {
					removable = false
					break
				}
			}
			if !removable {
				continue
			}
			recursive := false
			for child := range current {
				if strings.HasPrefix(child, name+"/") {
					recursive = true
					delete(current, child)
				}
			}
			delete(current, name)
			appendChange(Delete, name, "", Node{Identity: node.Identity, Kind: node.Kind}, recursive)
			progress = true
		}
		for _, name := range names {
			desired := after.Nodes[name]
			if actual, ok := current[name]; ok && actual.Identity == desired.Identity {
				continue
			}
			if _, occupied := current[name]; occupied || !parentReady(name) {
				continue
			}
			oldPath := ""
			for old, node := range current {
				if node.Identity == desired.Identity {
					oldPath = old
					break
				}
			}
			if oldPath != "" {
				if strings.HasPrefix(name, oldPath+"/") {
					return nil, execenv.ErrLagged
				}
				moved := make(map[string]Node)
				for old, node := range current {
					if old == oldPath || strings.HasPrefix(old, oldPath+"/") {
						destination := name + strings.TrimPrefix(old, oldPath)
						if execenv.ValidatePath(destination) != nil {
							return nil, execenv.ErrLagged
						}
						if _, occupied := current[destination]; occupied {
							return nil, execenv.ErrLagged
						}
						moved[destination] = node
					}
				}
				node := current[oldPath]
				for old := range current {
					if old == oldPath || strings.HasPrefix(old, oldPath+"/") {
						delete(current, old)
					}
				}
				maps.Copy(current, moved)
				appendChange(Move, oldPath, name, Node{Identity: node.Identity, Kind: node.Kind}, false)
			} else {
				if seen[desired.Identity] {
					return nil, execenv.ErrLagged
				}
				op := CreateFile
				if desired.Kind == execenv.ProjectionDirectory {
					op = CreateDirectory
				}
				current[name] = desired
				appendChange(op, name, "", desired, false)
			}
			progress = true
		}
		matched := len(current) == len(after.Nodes)
		for name, node := range current {
			if after.Nodes[name].Identity != node.Identity {
				matched = false
				break
			}
		}
		if matched {
			for _, name := range names {
				actual, desired := current[name], after.Nodes[name]
				if actual.Kind == execenv.ProjectionFile && !bytes.Equal(actual.Data, desired.Data) {
					appendChange(WriteFile, name, "", desired, false)
				}
			}
			return changes, nil
		}
		if !progress {
			return nil, execenv.ErrLagged
		}
	}
	return nil, execenv.ErrLagged
}
