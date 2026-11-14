package execenv

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// MaxTreeEntries is the maximum number of entries in one projection.
	MaxTreeEntries = 500
	// MaxFileBytes is the maximum size of one file body.
	MaxFileBytes = 10 << 20
	// MaxTreeBytes is the maximum sum of file bodies in one projection.
	MaxTreeBytes = 50 << 20
	// MaxPathBytes is the maximum UTF-8 length of a path.
	MaxPathBytes = 1024
	// MaxPathSegments is the maximum number of '/' separated components.
	MaxPathSegments = 16
	// MaxSegmentBytes is the maximum UTF-8 length of one path component.
	MaxSegmentBytes = 255
)

// Version is an opaque caller token for one file's current content.
// The host compares it for equality and returns it; it never parses it.
// Directories do not have a version.
type Version string

// Tree, Node, Mutation and Batch are the physical file vocabulary an
// adapter commits for a journal projection. Callers speak
// ProjectionMutation; adapters translate it into these values.

// NodeKind is a file or directory in a projected tree.
type NodeKind uint8

const (
	// KindFile is a regular file.
	KindFile NodeKind = iota
	// KindDirectory is an empty-or-parent directory. Directory bytes are
	// never stored; the path itself is the only directory state.
	KindDirectory
)

// Node is one path in a complete physical tree. Data is the file body; an
// empty non-nil slice is an empty file. Directories have nil Data and an
// empty Version.
type Node struct {
	Path    string
	Kind    NodeKind
	Version Version
	Data    []byte
}

// Tree is a complete physical snapshot: listed paths exist, unlisted paths
// do not.
type Tree []Node

// Op is one physical filesystem change.
type Op uint8

const (
	// OpCreate adds a path that did not exist.
	OpCreate Op = iota + 1
	// OpReplace changes the body of an existing file.
	OpReplace
	// OpMove renames a path. From is the old path, Path is the new one.
	OpMove
	// OpDelete removes a path.
	OpDelete
)

// Mutation is one physical change in a Batch.
//
// Expected is an optimistic fence for files. Empty means "apply
// unconditionally". A mismatch is ErrConflict.
type Mutation struct {
	// Recursive is required to delete a nonempty directory. It is invalid
	// for other operations or for file deletion.
	Recursive bool
	Op        Op
	Path      string
	From      string
	Kind      NodeKind
	Version   Version
	Expected  Version
	Data      []byte
}

// Batch is an atomic group of mutations: applied completely or not at all.
type Batch struct {
	Mutations []Mutation
}

// ValidatePath reports ErrInvalid when path is not a bounded POSIX-relative
// location. Product-specific reserved roots are the caller's problem.
func ValidatePath(path string) error {
	if path == "" || !utf8.ValidString(path) || len(path) > MaxPathBytes {
		return ErrInvalid
	}
	if path[0] == '/' || strings.Contains(path, "\\") || strings.ContainsRune(path, 0) {
		return ErrInvalid
	}
	if strings.HasSuffix(path, "/") {
		return ErrInvalid
	}
	parts := strings.Split(path, "/")
	if len(parts) > MaxPathSegments {
		return ErrInvalid
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return ErrInvalid
		}
		if len(part) > MaxSegmentBytes {
			return ErrInvalid
		}
		for i := 0; i < len(part); i++ {
			if part[i] < 0x20 {
				return ErrInvalid
			}
		}
	}
	return nil
}

// ResolvePath joins root and a validated relative path. The result is
// always inside root; a cleaned escape is ErrInvalid.
func ResolvePath(root, path string) (string, error) {
	if err := ValidatePath(path); err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(path))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrInvalid
	}
	return full, nil
}
