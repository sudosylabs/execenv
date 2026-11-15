package execenv

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// MaxPathBytes is the maximum UTF-8 length of a path.
	MaxPathBytes = 4096
	// MaxPathSegments is the maximum number of '/' separated components.
	MaxPathSegments = 64
	// MaxSegmentBytes is the maximum UTF-8 length of one path component.
	MaxSegmentBytes = 255
)

// Version is an opaque caller token for one file's current content.
// The host compares it for equality and returns it; it never parses it.
// Directories do not have a version.
type Version string

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
