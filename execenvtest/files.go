package execenvtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/archive"
)

func runFiles(t *testing.T, factory Factory) {

	t.Run("a broken write resumes from what the host holds", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "resume-write")
		ctx := t.Context()
		body := bytes.Repeat([]byte("0123456789abcdef"), 512<<10) // 8 MiB
		const cut = 3 << 20
		broken := io.MultiReader(bytes.NewReader(body[:cut]), failingReader{})
		opts := execenv.WriteOptions{TransferID: "upload-1"}
		if _, err := env.WriteFile(ctx, fence, "big.bin", broken, opts); err == nil {
			t.Fatal("a write whose body failed succeeded")
		}
		if _, err := env.Stat(ctx, "big.bin", false); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("a broken write left a file: %v", err)
		}
		// The write returns once the host has kept what arrived: all of it.
		held, err := env.TransferOffset(ctx, "upload-1")
		if err != nil || held != cut {
			t.Fatalf("TransferOffset() = %d, %v; want %d", held, err, cut)
		}
		wrong := opts
		wrong.Offset = held + 1
		if _, err := env.WriteFile(ctx, fence, "big.bin", bytes.NewReader(body[held+1:]), wrong); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("resume at the wrong offset = %v, want ErrConflict", err)
		}
		opts.Offset = held
		info, err := env.WriteFile(ctx, fence, "big.bin", bytes.NewReader(body[held:]), opts)
		sum := sha256.Sum256(body)
		if err != nil || info.Size != int64(len(body)) || info.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("resumed write = %+v, %v", info, err)
		}
		if after, err := env.TransferOffset(ctx, "upload-1"); err != nil || after != 0 {
			t.Fatalf("TransferOffset() after it landed = %d, %v", after, err)
		}
		// Without a transfer id a broken write keeps nothing.
		if _, err := env.WriteFile(ctx, fence, "other.bin", io.MultiReader(bytes.NewReader(body[:1024]), failingReader{}), execenv.WriteOptions{}); err == nil {
			t.Fatal("a plain write whose body failed succeeded")
		}
		if _, err := env.Stat(ctx, "other.bin", false); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("a broken plain write left a file: %v", err)
		}
		if _, err := env.TransferOffset(ctx, "bad id!"); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("TransferOffset() of a bad id = %v, want ErrInvalid", err)
		}
	})
	t.Run("the file API writes, reads, lists, moves and removes", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "files")
		ctx := t.Context()
		if err := env.MakeDirectory(ctx, fence, "a/b", true); err != nil {
			t.Fatalf("MakeDirectory(parents) = %v", err)
		}
		if err := env.MakeDirectory(ctx, fence, "a", false); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("MakeDirectory(existing) = %v, want ErrConflict", err)
		}
		info, err := env.WriteFile(ctx, fence, "a/b/note.txt", strings.NewReader("hello"), execenv.WriteOptions{ExpectedSHA256: execenv.ExpectAbsent})
		if err != nil || info.Size != 5 || info.SHA256 == "" {
			t.Fatalf("WriteFile() = %+v, %v", info, err)
		}
		if _, err := env.WriteFile(ctx, fence, "a/b/note.txt", strings.NewReader("x"), execenv.WriteOptions{ExpectedSHA256: execenv.ExpectAbsent}); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("WriteFile(absent) over a file = %v, want ErrConflict", err)
		}
		if _, err := env.WriteFile(ctx, fence, "a/b/note.txt", strings.NewReader("x"), execenv.WriteOptions{ExpectedSHA256: strings.Repeat("0", 64)}); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("WriteFile(wrong digest) = %v, want ErrConflict", err)
		}
		if _, err := env.WriteFile(ctx, fence, "a/b/note.txt", strings.NewReader("hello, again"), execenv.WriteOptions{ExpectedSHA256: info.SHA256}); err != nil {
			t.Fatalf("WriteFile(right digest) = %v", err)
		}
		stale := fence
		stale.FenceRevision++
		if _, err := env.WriteFile(ctx, stale, "a/stale", strings.NewReader("x"), execenv.WriteOptions{}); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("WriteFile(stale fence) = %v, want ErrConflict", err)
		}
		stat, err := env.Stat(ctx, "a/b/note.txt", true)
		if err != nil || stat.Size != 12 || stat.SHA256 == "" || stat.Dir {
			t.Fatalf("Stat() = %+v, %v", stat, err)
		}
		body, err := env.ReadFile(ctx, "a/b/note.txt", 7)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(body)
		_ = body.Close()
		if string(got) != "again" {
			t.Fatalf("ReadFile(offset 7) = %q", got)
		}
		if _, err := env.ReadFile(ctx, "missing", 0); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("ReadFile(missing) = %v, want ErrNotFound", err)
		}
		list, err := env.List(ctx, "", 3, false)
		if err != nil || len(list) != 3 || list[0].Path != "a" || list[2].Path != "a/b/note.txt" {
			t.Fatalf("List() = %+v, %v", list, err)
		}
		if err := env.Move(ctx, fence, "a/b", "c"); err != nil {
			t.Fatal(err)
		}
		if _, err := env.Stat(ctx, "c/note.txt", false); err != nil {
			t.Fatalf("Stat() after Move = %v", err)
		}
		if err := env.Remove(ctx, fence, "c", false); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Remove(non-empty, not recursive) = %v, want ErrConflict", err)
		}
		if err := env.Remove(ctx, fence, "c", true); err != nil {
			t.Fatal(err)
		}
		if _, err := env.Stat(ctx, "c", false); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("Stat() after Remove = %v", err)
		}
	})

	t.Run("import is all or nothing and export round-trips", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "archives")
		ctx := t.Context()
		raw, digest := seedArchive(t)
		broken := io.MultiReader(bytes.NewReader(raw), failingReader{})
		if err := env.Import(ctx, fence, "", broken); err == nil {
			t.Fatal("Import() succeeded after the source failed")
		}
		if _, err := env.Stat(ctx, "stale.txt", false); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("Stat() after interrupted Import = %v, want ErrNotFound", err)
		}
		if err := env.Import(ctx, fence, "", bytes.NewReader(raw)); err != nil {
			t.Fatalf("Import() = %v", err)
		}
		if err := env.Import(ctx, fence, "", bytes.NewReader(raw)); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Import() over existing paths = %v, want ErrConflict", err)
		}
		out, err := env.Export(ctx, []string{"src", "stale.txt"}, string(archive.CodecGzip))
		if err != nil {
			t.Fatal(err)
		}
		got, err := archive.Read(out, func(archive.Entry, io.Reader) error { return nil })
		_ = out.Close()
		if err != nil || got != digest {
			t.Fatalf("Export() digest = %s, %v, want %s", got, err, digest)
		}
	})

	t.Run("file API writes are observed like guest changes", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "observed-files")
		if _, err := env.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init"}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.WriteFile(t.Context(), fence, "out.txt", strings.NewReader("result"), execenv.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		page, err := env.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: 0, Limit: 128})
		if err != nil || len(page.Observations) != 1 || page.Observations[0].Path != "out.txt" || page.Observations[0].Operation != execenv.ObservationCreateFile {
			t.Fatalf("Observe() = %+v, %v", page, err)
		}
	})
}

// failingReader fails every read, as a caller's broken source does.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("source broke") }
