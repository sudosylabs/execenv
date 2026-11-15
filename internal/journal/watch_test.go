//go:build linux || darwin

package journal

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeWatcher struct {
	paths   []string
	all     bool
	writers map[string]string
}

func (w *fakeWatcher) Take() Changes {
	taken := Changes{Paths: w.paths, All: w.all, Writers: w.writers}
	w.paths, w.all, w.writers = nil, false, nil
	return taken
}

// With a watcher, Observe reads only the paths it names, and nothing when
// it names none.
func TestObserveReadsOnlyWhatTheWatcherNames(t *testing.T) {
	j, fence, _ := journalFixture(t)
	w := &fakeWatcher{}
	j.Watch(w)
	if _, err := j.Observe(t.Context(), fence, 0, 128); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir/named.txt", "dir/unnamed.txt"} {
		if err := os.WriteFile(filepath.Join(j.home, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	page, err := j.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 0 {
		t.Fatalf("Observe() read though the watcher named nothing: %+v %v", page, err)
	}
	w.paths = []string{"dir/named.txt"}
	page, err = j.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Path != "dir/named.txt" {
		t.Fatalf("Observe() of one named path = %+v %v", page, err)
	}
	// When the watcher cannot say, the whole workspace is walked.
	w.all = true
	page, err = j.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 2 || page.Observations[1].Path != "dir/unnamed.txt" {
		t.Fatalf("Observe() after an unnamed change = %+v %v", page, err)
	}
}

// Each change names the tag the watcher gave its path's last writer; a
// move names its renamer, seen at the new name.
func TestObservationsNameTheWatchersWriters(t *testing.T) {
	j, fence, _ := journalFixture(t)
	w := &fakeWatcher{}
	j.Watch(w)
	if _, err := j.Observe(t.Context(), fence, 0, 128); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.home, "dir/new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(j.home, "dir/file"), filepath.Join(j.home, "dir/moved")); err != nil {
		t.Fatal(err)
	}
	w.all = true
	w.writers = map[string]string{"dir/new.txt": "build", "dir/file": "shell", "dir/moved": "editor"}
	page, err := j.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 2 {
		t.Fatalf("Observe() = %+v %v", page, err)
	}
	for _, event := range page.Observations {
		want := map[string]string{"dir/new.txt": "build", "dir/file": "editor"}[event.Path]
		if event.ProcessTag != want {
			t.Fatalf("%s %s names %q, want %q", event.Operation, event.Path, event.ProcessTag, want)
		}
	}
}
