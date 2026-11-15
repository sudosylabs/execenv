//go:build linux || darwin

package journal

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeWatcher struct {
	paths []string
	all   bool
}

func (w *fakeWatcher) Take() ([]string, bool) {
	paths, all := w.paths, w.all
	w.paths, w.all = nil, false
	return paths, all
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
