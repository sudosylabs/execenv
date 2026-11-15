package execenv

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Seed is a prepared workspace many grants start from. A host builds it
// once from an archive (see package archive) and gives each grant created
// with it a copy of its own; nothing of it crosses the network per grant.
type Seed struct {
	// Digest is the tree's digest, archive.Digest of its manifest.
	Digest string
	// Bytes is the size of its files, and Entries how many paths it has.
	Bytes   int64
	Entries int64
}

// Seeder is the part of a Host that keeps seeds. Every Host is one.
type Seeder interface {
	// PutSeed builds the seed an archive holds and names it by digest. The
	// host refuses an archive whose tree has another digest, or holds
	// anything but directories and regular files. Putting a seed the host
	// already has returns it without reading past the archive's header.
	PutSeed(ctx context.Context, digest string, archive io.Reader) (Seed, error)
	// Seeds lists the seeds this host holds.
	Seeds(ctx context.Context) ([]Seed, error)
	// DeleteSeed removes a seed. Grants already created from it keep their
	// copy.
	DeleteSeed(ctx context.Context, digest string) error
}

// MissingContentError is the ErrMissingContent an initialization returns
// when files it names are neither uploaded nor already in the grant's seed.
// It changes nothing; upload those and initialize again.
type MissingContentError struct {
	Paths []string
}

func (e *MissingContentError) Error() string {
	return fmt.Sprintf("execenv: missing content for %d paths: %s", len(e.Paths), strings.Join(e.Paths, ", "))
}

func (e *MissingContentError) Is(target error) bool { return target == ErrMissingContent }

// ValidateSeedDigest refuses anything but a lowercase hex SHA-256.
func ValidateSeedDigest(digest string) error {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return ErrInvalid
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrInvalid
		}
	}
	return nil
}
