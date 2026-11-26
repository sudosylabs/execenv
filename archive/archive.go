// Package archive writes and reads the tree archives execenv takes as
// seeds: a short header naming the compression, then a tar stream of
// directories and regular files only.
//
// A tree's digest is the SHA-256 of its canonical manifest: every entry
// sorted by path, each with its kind, whether it is executable, its size
// and its content's SHA-256. Two trees with the same files have the same
// digest however they were archived or compressed, so a caller can name a
// seed before building it, and a host can verify one while unpacking it.
package archive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/sudosylabs/execenv"
)

// Codec is how an archive's tar stream is compressed.
type Codec string

const (
	CodecNone Codec = "none"
	CodecGzip Codec = "gzip"
	// CodecZstd is the default: several times faster to unpack than gzip
	// at a similar ratio.
	CodecZstd Codec = "zstd"
)

// magic opens every archive; the codec's name follows on the same line.
const magic = "execenv-archive-1 "

// Entry is one path of a tree.
type Entry struct {
	Path       string
	Dir        bool
	Executable bool
	Size       int64
	// SHA256 is a file's content digest, lowercase hex. Directories have
	// none.
	SHA256 string
}

// Digest is the tree's digest: the SHA-256 of its canonical manifest.
func Digest(entries []Entry) string {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	sum := sha256.New()
	for _, e := range sorted {
		kind := "file"
		if e.Dir {
			kind = "dir"
		}
		fmt.Fprintf(sum, "%s\t%s\t%t\t%d\t%s\n", kind, e.Path, e.Executable, e.Size, e.SHA256)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// Writer builds an archive. Add every directory before the paths under it.
type Writer struct {
	tar     *tar.Writer
	closer  io.Closer
	entries []Entry
	seen    map[string]bool
}

// NewWriter starts an archive on w.
func NewWriter(w io.Writer, codec Codec) (*Writer, error) {
	if _, err := io.WriteString(w, magic+string(codec)+"\n"); err != nil {
		return nil, err
	}
	var body io.Writer
	var closer io.Closer
	switch codec {
	case CodecNone:
		body = w
	case CodecGzip:
		g := gzip.NewWriter(w)
		body, closer = g, g
	case CodecZstd:
		z, err := zstd.NewWriter(w)
		if err != nil {
			return nil, err
		}
		body, closer = z, z
	default:
		return nil, execenv.ErrInvalid
	}
	return &Writer{tar: tar.NewWriter(body), closer: closer, seen: make(map[string]bool)}, nil
}

func (w *Writer) admit(name string, directory bool) error {
	if _, seen := w.seen[name]; execenv.ValidatePath(name) != nil || seen {
		return execenv.ErrInvalid
	}
	if parent := path.Dir(name); parent != "." && !w.seen[parent] {
		return execenv.ErrInvalid
	}
	w.seen[name] = directory
	return nil
}

// Dir adds a directory.
func (w *Writer) Dir(name string) error {
	if err := w.admit(name, true); err != nil {
		return err
	}
	w.entries = append(w.entries, Entry{Path: name, Dir: true})
	return w.tar.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755})
}

// File adds a file of size bytes read from body.
func (w *Writer) File(name string, executable bool, size int64, body io.Reader) error {
	if err := w.admit(name, false); err != nil {
		return err
	}
	mode := int64(0o644)
	if executable {
		mode = 0o755
	}
	if err := w.tar.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: size}); err != nil {
		return err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(w.tar, sum), io.LimitReader(body, size))
	if err != nil {
		return err
	}
	if n != size {
		return execenv.ErrInvalid
	}
	w.entries = append(w.entries, Entry{Path: name, Executable: executable, Size: size, SHA256: hex.EncodeToString(sum.Sum(nil))})
	return nil
}

// Close finishes the archive and returns the tree's digest.
func (w *Writer) Close() (string, error) {
	if err := w.tar.Close(); err != nil {
		return "", err
	}
	if w.closer != nil {
		if err := w.closer.Close(); err != nil {
			return "", err
		}
	}
	return Digest(w.entries), nil
}

// Visit is called for each entry in order. A file's body is read from body;
// it need not be read whole.
type Visit func(entry Entry, body io.Reader) error

// Read reads an archive, calling visit for every entry, and returns the
// tree's digest. Anything but a directory or a regular file, an invalid or
// repeated path, or a path whose parent directory was not listed first is
// execenv.ErrInvalid, and reading stops there.
func Read(r io.Reader, visit Visit) (string, error) {
	br := bufio.NewReader(r)
	header, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(header, magic) {
		return "", execenv.ErrInvalid
	}
	var body io.Reader
	switch Codec(strings.TrimSuffix(strings.TrimPrefix(header, magic), "\n")) {
	case CodecNone:
		body = br
	case CodecGzip:
		g, err := gzip.NewReader(br)
		if err != nil {
			return "", execenv.ErrInvalid
		}
		defer g.Close()
		body = g
	case CodecZstd:
		z, err := zstd.NewReader(br)
		if err != nil {
			return "", execenv.ErrInvalid
		}
		defer z.Close()
		body = z
	default:
		return "", execenv.ErrInvalid
	}
	tr := tar.NewReader(body)
	seen := make(map[string]bool)
	var entries []Entry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// Read through the compression footer so a checksum error or
			// interrupted source cannot be hidden by the tar end marker.
			if _, err := io.Copy(io.Discard, body); err != nil {
				return "", execenv.ErrInvalid
			}
			return Digest(entries), nil
		}
		if err != nil {
			return "", execenv.ErrInvalid
		}
		name := strings.TrimSuffix(h.Name, "/")
		if _, duplicate := seen[name]; execenv.ValidatePath(name) != nil || duplicate {
			return "", execenv.ErrInvalid
		}
		if parent := path.Dir(name); parent != "." && !seen[parent] {
			return "", execenv.ErrInvalid
		}
		seen[name] = h.Typeflag == tar.TypeDir
		var entry Entry
		switch h.Typeflag {
		case tar.TypeDir:
			entry = Entry{Path: name, Dir: true}
			if err := visit(entry, nil); err != nil {
				return "", err
			}
		case tar.TypeReg:
			entry = Entry{Path: name, Executable: h.Mode&0o111 != 0, Size: h.Size}
			sum := sha256.New()
			tee := io.TeeReader(tr, sum)
			if err := visit(entry, tee); err != nil {
				return "", err
			}
			// Whatever visit left unread still counts toward the digest.
			if _, err := io.Copy(io.Discard, tee); err != nil {
				return "", execenv.ErrInvalid
			}
			entry.SHA256 = hex.EncodeToString(sum.Sum(nil))
		default:
			return "", execenv.ErrInvalid
		}
		entries = append(entries, entry)
	}
}
