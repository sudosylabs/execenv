package content

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPutStoresOnceByDigestAndPlaceCopies(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := store.Put(strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Put(strings.NewReader("same"))
	if err != nil || a != b || a.Size != 4 {
		t.Fatalf("Put twice = %+v %+v %v", a, b, err)
	}
	if hashed, _ := Hash(strings.NewReader("same")); hashed != a {
		t.Fatal("Hash and Put disagree")
	}
	dest := filepath.Join(t.TempDir(), "file")
	if err := store.Place(a, dest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := store.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	body, _ := io.ReadAll(f)
	if string(body) != "same" {
		t.Fatal("writing a placed file changed the stored body")
	}
}

func TestSweepKeepsLiveAndRecentBodies(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	live, _ := store.Put(strings.NewReader("live"))
	dead, _ := store.Put(strings.NewReader("dead"))
	recent, _ := store.Put(strings.NewReader("recent"))
	old := time.Now().Add(-2 * sweepGrace)
	for _, b := range []Blob{live, dead} {
		if err := os.Chtimes(filepath.Join(dir, b.Digest), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Sweep(map[string]bool{live.Digest: true}); err != nil {
		t.Fatal(err)
	}
	if !store.Has(live) || store.Has(dead) || !store.Has(recent) {
		t.Fatalf("after sweep: live=%v dead=%v recent=%v", store.Has(live), store.Has(dead), store.Has(recent))
	}
}

func TestLargeBodyStreamsWithoutLimit(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	b, err := store.Put(io.LimitReader(zeros{}, size))
	if err != nil || b.Size != size {
		t.Fatalf("Put(64 MiB) = %+v %v", b, err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	copy(p, bytes.Repeat([]byte{0}, len(p)))
	return len(p), nil
}
