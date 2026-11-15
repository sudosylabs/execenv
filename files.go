package execenv

import (
	"context"
	"io"
	"time"
)

// FileInfo describes one path in a grant's workspace.
type FileInfo struct {
	Path       string
	Dir        bool
	Executable bool
	Size       int64
	Modified   time.Time
	// SHA256 is a file's content digest, filled when the caller asked for
	// it. Directories have none.
	SHA256 string
}

// WriteOptions shape a WriteFile.
type WriteOptions struct {
	// ExpectedSHA256, when set, refuses the write with ErrConflict unless
	// the file now has that digest. "absent" requires that no file exists.
	ExpectedSHA256 string
	Executable     bool
	// TransferID, when set, makes the write resumable: the bytes of a
	// broken stream are kept under it, and a later WriteFile with the same
	// id and Offset at TransferOffset carries on. The caller picks it, in
	// the id alphabet. Kept bytes expire an hour after they last grew.
	TransferID string
	// Offset is where this body starts within the transfer: what
	// TransferOffset reports. Anything else is ErrConflict. Without a
	// TransferID it must be 0.
	Offset int64
}

// TransferExpiry is how long a broken transfer's bytes are kept.
const TransferExpiry = time.Hour

// ValidateTransferID checks a caller's transfer id.
func ValidateTransferID(id string) error {
	if len(id) > 64 || validateName(id) != nil {
		return ErrInvalid
	}
	return nil
}

// ExpectAbsent is the ExpectedSHA256 that requires the path not to exist.
const ExpectAbsent = "absent"

// MaxListEntries bounds one List.
const MaxListEntries = 100_000

// Files is a plain file API over a grant's workspace, for callers that do
// not keep durable authority elsewhere. Its writes are guest changes like
// any other: once a projection is initialized they appear in Observe.
// Paths are workspace-relative and checked by ValidatePath; "" is the
// workspace itself where a directory is meant.
type Files interface {
	Stat(ctx context.Context, path string, digest bool) (FileInfo, error)
	// List returns dir's entries, and their descendants to depth levels
	// (1 is dir's own entries), sorted by path.
	List(ctx context.Context, dir string, depth int, digest bool) ([]FileInfo, error)
	// ReadFile streams a file from offset.
	ReadFile(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
	// WriteFile streams body into place. It replaces the file only once the
	// whole body has arrived, so a broken stream leaves the file as it was.
	WriteFile(ctx context.Context, fence ProjectionFence, path string, body io.Reader, opts WriteOptions) (FileInfo, error)
	// TransferOffset is how many bytes of a resumable write are kept: the
	// Offset a retry starts from. An unknown or expired transfer is 0.
	TransferOffset(ctx context.Context, transferID string) (int64, error)
	MakeDirectory(ctx context.Context, fence ProjectionFence, path string, parents bool) error
	// Move renames from to to. The destination must not exist.
	Move(ctx context.Context, fence ProjectionFence, from, to string) error
	Remove(ctx context.Context, fence ProjectionFence, path string, recursive bool) error
	// Import unpacks an archive (see package archive) under dir. Either
	// every entry lands or none does; an entry that already exists is
	// ErrConflict.
	Import(ctx context.Context, fence ProjectionFence, dir string, archive io.Reader) error
	// Export streams an archive of the given paths, compressed with codec
	// ("none", "gzip" or "zstd").
	Export(ctx context.Context, paths []string, codec string) (io.ReadCloser, error)
}
