//go:build linux || darwin

package journal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sudosylabs/execenv"
)

// A body the guest writes is reported whole however large, read from the
// workspace, and stale once the guest changes it.
func TestObservedBodyIsReadLiveAndStaleOnceChanged(t *testing.T) {
	j, fence, _ := journalFixture(t)
	body := bytes.Repeat([]byte("guest-bytes-"), (24<<20)/12)
	path := filepath.Join(j.home, "dir/large.bin")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := j.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Operation != execenv.ObservationCreateFile {
		t.Fatalf("observe = %+v %v", page, err)
	}
	ref := *page.Observations[0].Content
	if ref.Size != int64(len(body)) {
		t.Fatalf("reported size = %d, want %d", ref.Size, len(body))
	}
	if _, err := os.Stat(filepath.Join(j.store.Dir(), ref.Digest)); err == nil {
		t.Fatal("observing copied the body into the store")
	}
	reader, err := j.Content(fence, ref)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("observed body was cut: %d bytes, %v", len(got), err)
	}

	// Same size, other bytes: only the hash tells, at the end.
	changed := bytes.Clone(body)
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err = j.Content(fence, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, execenv.ErrStale) {
		t.Fatalf("changed body read = %v, want ErrStale", err)
	}
	_ = reader.Close()

	// Another size or a symlink in its place is stale at once.
	if err := os.WriteFile(path, []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Content(fence, ref); !errors.Is(err, execenv.ErrStale) {
		t.Fatalf("resized body = %v, want ErrStale", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", path); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Content(fence, ref); !errors.Is(err, execenv.ErrStale) {
		t.Fatalf("symlinked body = %v, want ErrStale", err)
	}

	if err := j.Acknowledge(fence, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Content(fence, ref); err == nil {
		t.Fatal("acknowledged body still readable")
	}
}
