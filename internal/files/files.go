// Package files serves the plain file API over one workspace directory.
//
// Every path is opened beneath the workspace's root, so no path, link or
// rename can reach outside it. Writes and imports stage on the same disk
// and land by rename: a broken stream changes nothing.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/archive"
	"github.com/sudosylabs/execenv/internal/content"
)

// FS is one workspace.
type FS struct {
	home  string
	stage string
	// Own, when set, hands a created path to the workload's user.
	Own func(string) error
	// transfers holds a lock per resumable transfer.
	transfers sync.Map
}

// New serves home, staging under stage, which must be on the same disk and
// outside home.
func New(home, stage string) (*FS, error) {
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return nil, err
	}
	return &FS{home: home, stage: stage}, nil
}

func (f *FS) resolve(name string) (string, error) {
	if name == "" {
		return f.home, nil
	}
	return execenv.ResolvePath(f.home, name)
}

func notFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return execenv.ErrNotFound
	}
	return content.DiskError(err)
}

func (f *FS) info(name, full string, digest bool) (execenv.FileInfo, error) {
	st, err := os.Lstat(full)
	if err != nil {
		return execenv.FileInfo{}, notFound(err)
	}
	if !st.IsDir() && !st.Mode().IsRegular() {
		return execenv.FileInfo{}, execenv.ErrInvalid
	}
	out := execenv.FileInfo{Path: name, Dir: st.IsDir(), Executable: st.Mode()&0o111 != 0 && !st.IsDir(), Modified: st.ModTime()}
	if !st.IsDir() {
		out.Size = st.Size()
		if digest {
			file, err := os.Open(full)
			if err != nil {
				return execenv.FileInfo{}, notFound(err)
			}
			blob, err := content.Hash(file)
			_ = file.Close()
			if err != nil {
				return execenv.FileInfo{}, err
			}
			out.SHA256 = blob.Digest
		}
	}
	return out, nil
}

// Stat describes one path.
func (f *FS) Stat(name string, digest bool) (execenv.FileInfo, error) {
	full, err := f.resolve(name)
	if err != nil {
		return execenv.FileInfo{}, err
	}
	return f.info(name, full, digest)
}

// List walks dir to depth.
func (f *FS) List(dir string, depth int, digest bool) ([]execenv.FileInfo, error) {
	if depth < 1 {
		depth = 1
	}
	full, err := f.resolve(dir)
	if err != nil {
		return nil, err
	}
	var out []execenv.FileInfo
	var walk func(rel, abs string, level int) error
	walk = func(rel, abs string, level int) error {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return notFound(err)
		}
		for _, e := range entries {
			name := e.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			info, err := f.info(childRel, filepath.Join(abs, name), digest)
			if errors.Is(err, execenv.ErrInvalid) {
				continue // links and devices are not workspace files
			}
			if err != nil {
				return err
			}
			if len(out) >= execenv.MaxListEntries {
				return execenv.ErrTooLarge
			}
			out = append(out, info)
			if info.Dir && level < depth {
				if err := walk(childRel, filepath.Join(abs, name), level+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(dir, full, 1); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b execenv.FileInfo) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// ReadFile opens a regular file at offset.
func (f *FS) ReadFile(name string, offset int64) (io.ReadCloser, error) {
	full, err := f.resolve(name)
	if err != nil || name == "" {
		return nil, execenv.ErrInvalid
	}
	st, err := os.Lstat(full)
	if err != nil {
		return nil, notFound(err)
	}
	if !st.Mode().IsRegular() || offset < 0 || offset > st.Size() {
		return nil, execenv.ErrInvalid
	}
	file, err := os.Open(full)
	if err != nil {
		return nil, notFound(err)
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// WriteFile stages body, checks the expected digest, then renames it into
// place.
func (f *FS) WriteFile(name string, body io.Reader, opts execenv.WriteOptions) (execenv.FileInfo, error) {
	full, err := f.resolve(name)
	if err != nil || name == "" {
		return execenv.FileInfo{}, execenv.ErrInvalid
	}
	if st, err := os.Lstat(filepath.Dir(full)); err != nil || !st.IsDir() {
		return execenv.FileInfo{}, execenv.ErrNotFound
	}
	var stage *os.File
	if opts.TransferID == "" {
		if opts.Offset != 0 {
			return execenv.FileInfo{}, execenv.ErrInvalid
		}
		if stage, err = os.CreateTemp(f.stage, "write-*"); err != nil {
			return execenv.FileInfo{}, content.DiskError(err)
		}
	} else {
		if execenv.ValidateTransferID(opts.TransferID) != nil {
			return execenv.FileInfo{}, execenv.ErrInvalid
		}
		// Held for the whole write: a TransferOffset asked as a broken
		// stream is still landing waits for its last bytes.
		unlock := f.lockTransfer(opts.TransferID)
		defer unlock()
		f.expire()
		held, err := f.offset(opts.TransferID)
		if err != nil {
			return execenv.FileInfo{}, err
		}
		if opts.Offset != held {
			return execenv.FileInfo{}, execenv.ErrConflict
		}
		if stage, err = os.OpenFile(f.transfer(opts.TransferID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
			return execenv.FileInfo{}, content.DiskError(err)
		}
	}
	staged := stage.Name()
	_, err = io.Copy(stage, body)
	if cerr := stage.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// A broken transfer keeps what arrived for its retry.
		if opts.TransferID == "" {
			_ = os.Remove(staged)
		}
		return execenv.FileInfo{}, content.DiskError(err)
	}
	// The whole body is here: from now on the transfer is used up, whether
	// the write lands or is refused.
	defer os.Remove(staged)
	sum := sha256.New()
	size, err := hashFile(staged, sum)
	if err != nil {
		return execenv.FileInfo{}, content.DiskError(err)
	}
	if err := f.expect(full, opts.ExpectedSHA256); err != nil {
		return execenv.FileInfo{}, err
	}
	mode := os.FileMode(0o644)
	if opts.Executable {
		mode = 0o755
	}
	if err := os.Chmod(staged, mode); err != nil {
		return execenv.FileInfo{}, err
	}
	if err := f.own(staged); err != nil {
		return execenv.FileInfo{}, err
	}
	if err := os.Rename(staged, full); err != nil {
		return execenv.FileInfo{}, content.DiskError(err)
	}
	st, _ := os.Lstat(full)
	out := execenv.FileInfo{Path: name, Executable: opts.Executable, Size: size, SHA256: hex.EncodeToString(sum.Sum(nil))}
	if st != nil {
		out.Modified = st.ModTime()
	}
	return out, nil
}

// transfer is where a resumable write's bytes are kept.
func (f *FS) transfer(id string) string { return filepath.Join(f.stage, "transfer-"+id) }

// TransferOffset is how many bytes of transfer id are kept, once no write
// to it is under way.
func (f *FS) TransferOffset(id string) (int64, error) {
	if execenv.ValidateTransferID(id) != nil {
		return 0, execenv.ErrInvalid
	}
	unlock := f.lockTransfer(id)
	defer unlock()
	return f.offset(id)
}

// lockTransfer serializes the writes and reads of one transfer.
func (f *FS) lockTransfer(id string) func() {
	v, _ := f.transfers.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (f *FS) offset(id string) (int64, error) {
	st, err := os.Stat(f.transfer(id))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, content.DiskError(err)
	}
	if time.Since(st.ModTime()) > execenv.TransferExpiry {
		_ = os.Remove(f.transfer(id))
		return 0, nil
	}
	return st.Size(), nil
}

// expire drops transfers nothing has added to for TransferExpiry.
func (f *FS) expire() {
	entries, err := os.ReadDir(f.stage)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "transfer-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > execenv.TransferExpiry {
			_ = os.Remove(filepath.Join(f.stage, e.Name()))
		}
	}
}

// hashFile reads a staged body once to hash it and returns its size.
func hashFile(path string, sum io.Writer) (int64, error) {
	r, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	return io.Copy(sum, r)
}

func (f *FS) expect(full, want string) error {
	if want == "" {
		return nil
	}
	st, err := os.Lstat(full)
	if want == execenv.ExpectAbsent {
		if err == nil {
			return execenv.ErrConflict
		}
		return nil
	}
	if err != nil || !st.Mode().IsRegular() {
		return execenv.ErrConflict
	}
	file, err := os.Open(full)
	if err != nil {
		return execenv.ErrConflict
	}
	defer file.Close()
	blob, err := content.Hash(file)
	if err != nil || blob.Digest != want {
		return execenv.ErrConflict
	}
	return nil
}

func (f *FS) own(full string) error {
	if f.Own == nil {
		return nil
	}
	return f.Own(full)
}

// MakeDirectory makes one directory, or every missing parent too.
func (f *FS) MakeDirectory(name string, parents bool) error {
	full, err := f.resolve(name)
	if err != nil || name == "" {
		return execenv.ErrInvalid
	}
	if parents {
		parts := strings.Split(name, "/")
		for i := range parts {
			sub := filepath.Join(f.home, filepath.FromSlash(path.Join(parts[:i+1]...)))
			if err := os.Mkdir(sub, 0o755); err != nil {
				if st, serr := os.Lstat(sub); serr == nil && st.IsDir() {
					continue
				}
				return content.DiskError(err)
			}
			if err := f.own(sub); err != nil {
				return err
			}
		}
		return nil
	}
	if err := os.Mkdir(full, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return execenv.ErrConflict
		}
		return notFound(err)
	}
	return f.own(full)
}

// Move renames from to to.
func (f *FS) Move(from, to string) error {
	src, err := f.resolve(from)
	if err != nil || from == "" {
		return execenv.ErrInvalid
	}
	dst, err := f.resolve(to)
	if err != nil || to == "" || strings.HasPrefix(to+"/", from+"/") {
		return execenv.ErrInvalid
	}
	if _, err := os.Lstat(src); err != nil {
		return notFound(err)
	}
	if _, err := os.Lstat(dst); err == nil {
		return execenv.ErrConflict
	}
	if st, err := os.Lstat(filepath.Dir(dst)); err != nil || !st.IsDir() {
		return execenv.ErrNotFound
	}
	return content.DiskError(os.Rename(src, dst))
}

// Remove deletes a file, or a directory: an empty one, or any with
// recursive.
func (f *FS) Remove(name string, recursive bool) error {
	full, err := f.resolve(name)
	if err != nil || name == "" {
		return execenv.ErrInvalid
	}
	st, err := os.Lstat(full)
	if err != nil {
		return notFound(err)
	}
	if st.IsDir() && recursive {
		return os.RemoveAll(full)
	}
	if err := os.Remove(full); err != nil {
		if st.IsDir() {
			return execenv.ErrConflict
		}
		return err
	}
	return nil
}

// Import unpacks archive under dir, all or nothing.
func (f *FS) Import(dir string, r io.Reader) error {
	target, err := f.resolve(dir)
	if err != nil {
		return err
	}
	if st, err := os.Lstat(target); err != nil || !st.IsDir() {
		return execenv.ErrNotFound
	}
	stage, err := os.MkdirTemp(f.stage, "import-")
	if err != nil {
		return content.DiskError(err)
	}
	defer os.RemoveAll(stage)
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer root.Close()
	var tops []string
	_, err = archive.Read(r, func(e archive.Entry, body io.Reader) error {
		if !strings.Contains(e.Path, "/") {
			tops = append(tops, e.Path)
		}
		if e.Dir {
			return content.DiskError(root.Mkdir(e.Path, 0o755))
		}
		mode := os.FileMode(0o644)
		if e.Executable {
			mode = 0o755
		}
		out, err := root.OpenFile(e.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return content.DiskError(err)
		}
		_, err = io.Copy(out, body)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return content.DiskError(err)
	})
	if err != nil {
		if errors.Is(err, execenv.ErrQuota) {
			return err
		}
		return execenv.ErrInvalid
	}
	for _, top := range tops {
		if _, err := os.Lstat(filepath.Join(target, top)); err == nil {
			return execenv.ErrConflict
		}
	}
	if f.Own != nil {
		if err := filepath.WalkDir(stage, func(p string, _ fs.DirEntry, err error) error {
			if err != nil || p == stage {
				return err
			}
			return f.Own(p)
		}); err != nil {
			return err
		}
	}
	var moved []string
	for _, top := range tops {
		if err := os.Rename(filepath.Join(stage, top), filepath.Join(target, top)); err != nil {
			for _, back := range moved {
				_ = os.Rename(filepath.Join(target, back), filepath.Join(stage, back))
			}
			return content.DiskError(err)
		}
		moved = append(moved, top)
	}
	return nil
}

// Export writes an archive of paths to w.
func (f *FS) Export(paths []string, codec archive.Codec, w io.Writer) error {
	aw, err := archive.NewWriter(w, codec)
	if err != nil {
		return err
	}
	added := make(map[string]bool)
	var add func(name string) error
	add = func(name string) error {
		if added[name] {
			return nil
		}
		if parent := path.Dir(name); parent != "." && !added[parent] {
			if err := add(parent); err != nil {
				return err
			}
		}
		full, err := f.resolve(name)
		if err != nil {
			return err
		}
		st, err := os.Lstat(full)
		if err != nil {
			return notFound(err)
		}
		added[name] = true
		switch {
		case st.IsDir():
			return aw.Dir(name)
		case st.Mode().IsRegular():
			file, err := os.Open(full)
			if err != nil {
				return notFound(err)
			}
			defer file.Close()
			return aw.File(name, st.Mode()&0o111 != 0, st.Size(), file)
		}
		return nil
	}
	for _, name := range paths {
		if err := execenv.ValidatePath(name); err != nil {
			return err
		}
		full, _ := f.resolve(name)
		err := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return notFound(err)
			}
			rel, _ := filepath.Rel(f.home, p)
			return add(filepath.ToSlash(rel))
		})
		if err != nil {
			return err
		}
	}
	_, err = aw.Close()
	return err
}
