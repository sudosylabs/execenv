//go:build linux || darwin

package journal

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sudosylabs/execenv"
)

// A body the guest writes is reported whole however large, and stays as it
// was observed while the guest goes on writing.
func TestObservedBodyIsKeptWholeAndImmutable(t *testing.T) {
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
	if err := os.WriteFile(path, []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := j.Content(fence, ref)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("observed body changed or was cut: %d bytes, %v", len(got), err)
	}
	if err := j.Acknowledge(fence, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Content(fence, ref); err == nil {
		t.Fatal("acknowledged body still readable")
	}
}
