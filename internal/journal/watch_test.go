//go:build linux || darwin

package journal

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeWatcher struct{ changed bool }

func (w *fakeWatcher) Changed() bool {
	c := w.changed
	w.changed = false
	return c
}

// With a watcher, Observe walks the workspace only when it reports a change.
func TestObserveWalksOnlyWhenTheWatcherSaysSo(t *testing.T) {
	j, fence, _ := journalFixture(t)
	w := &fakeWatcher{}
	j.Watch(w)
	if _, err := j.Observe(t.Context(), fence, 0, 128); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.home, "dir/new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := j.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 0 {
		t.Fatalf("Observe() walked though the watcher saw nothing: %+v %v", page, err)
	}
	w.changed = true
	page, err = j.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Path != "dir/new.txt" {
		t.Fatalf("Observe() after a reported change = %+v %v", page, err)
	}
}
