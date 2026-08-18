package execenv

import (
	"context"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// MaxTreeEntries is the maximum number of nodes in one ReplaceTree or Apply.
	MaxTreeEntries = 500
	// MaxFileBytes is the maximum size of one file body.
	MaxFileBytes = 10 << 20
	// MaxTreeBytes is the maximum sum of file bodies in one ReplaceTree or Apply.
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

// NodeKind is a file or directory in a projected tree.
type NodeKind uint8

const (
	// KindFile is a regular file.
	KindFile NodeKind = iota
	// KindDirectory is an empty-or-parent directory. Directory bytes are
	// never stored; the path itself is the only directory state.
	KindDirectory
)

// Node is one path in a full snapshot passed to ReplaceTree.
//
// Data is the file body. A nil Data slice means "do not transfer": the host
// must already have Path at Version. An empty non-nil slice is an empty file.
// Directories ignore Data and require an empty Version.
type Node struct {
	Path    string
	Kind    NodeKind
	Version Version
	Data    []byte
}

// Tree is a complete snapshot. ReplaceTree makes the grant's projection
// match it exactly: listed paths exist, unlisted paths are deleted.
type Tree []Node

// Op is one guest-visible filesystem change.
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

// Mutation is one incremental change in an Apply batch.
//
// Expected is an optimistic fence for files. Empty means "apply
// unconditionally". A mismatch is ErrConflict. The whole batch is applied
// atomically or not at all.
type Mutation struct {
	Op       Op
	Path     string
	From     string
	Kind     NodeKind
	Version  Version
	Expected Version
	Data     []byte
}

// Batch is an atomic group of mutations.
type Batch struct {
	Mutations []Mutation
}

// Event is one guest-originated change. Caller-originated ReplaceTree and
// Apply calls do not appear here; Watch is how the host reports what the
// isolated environment wrote on its own.
type Event struct {
	Op   Op
	Path string
	From string
}

// Observation is a live stream of guest-originated events.
// Next blocks until an event, the context ends, or the stream fails closed
// (lag, freeze, revoke). After a non-nil error the observation is spent;
// open a new Watch after resynchronizing with ReplaceTree or Open.
type Observation interface {
	Next(ctx context.Context) (Event, error)
	Close() error
}

// GuestWriter is an optional adapter hook for tests. It mutates the
// projection as if the isolated environment did it, so Watch and Open can
// be exercised without a real guest agent. Production callers never need this.
type GuestWriter interface {
	WriteGuest(ctx context.Context, id ID, path string, data []byte) error
	RemoveGuest(ctx context.Context, id ID, path string) error
	MoveGuest(ctx context.Context, id ID, from, to string) error
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
