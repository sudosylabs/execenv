package agent

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanReusesHashWhenStatMatches(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "a.txt")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * racyWindow)
	first := scanHome(home, nil, nil)
	meta := first["a.txt"]
	if !meta.stat.trusted || meta.hash != sha256.Sum256([]byte("one")) {
		t.Fatalf("first scan = %+v", meta)
	}
	// A bogus previous hash with a matching stat key survives: the body was
	// not read again.
	meta.hash = sha256.Sum256([]byte("bogus"))
	first["a.txt"] = meta
	second := scanHome(home, first, nil)
	if second["a.txt"].hash != meta.hash {
		t.Fatal("unchanged file was rehashed")
	}
	// Same size and restored mtime still differ by change time.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	third := scanHome(home, second, nil)
	if third["a.txt"].hash != sha256.Sum256([]byte("two")) {
		t.Fatal("same-size rewrite with restored mtime was not rehashed")
	}
}

func TestScanDoesNotTrustRacyStat(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "fresh"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if scanHome(home, nil, nil)["fresh"].stat.trusted {
		t.Fatal("a file changed inside the racy window was trusted")
	}
}

func TestScanRegistersDirectoriesBeforeEntries(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	scanHome(home, nil, func(dir string) { dirs = append(dirs, dir) })
	want := []string{home, filepath.Join(home, "a"), filepath.Join(home, "a", "b")}
	if len(dirs) != len(want) {
		t.Fatalf("registered %v, want %v", dirs, want)
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Fatalf("registered %v, want %v", dirs, want)
		}
	}
}

func TestNotifierSignalsOnChange(t *testing.T) {
	home := t.TempDir()
	n := newNotifier()
	defer n.Close()
	n.Add(home)
	// Drain anything a tick fallback already queued.
	select {
	case <-n.Events():
	default:
	}
	if err := os.WriteFile(filepath.Join(home, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-n.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("notifier did not signal a change")
	}
}
