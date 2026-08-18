package execenv_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestResolvePathStaysInsideRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	got, err := execenv.ResolvePath(root, "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "src", "main.go")
	if got != want {
		t.Fatalf("ResolvePath = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, root) {
		t.Fatalf("ResolvePath = %q, want under %q", got, root)
	}
	if _, err := execenv.ResolvePath(root, "../escape"); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("escape error = %v, want ErrInvalid", err)
	}
}

func TestValidatePath(t *testing.T) {
	t.Parallel()
	ok := []string{"main.go", "src/main.go", "a/b/c"}
	for _, path := range ok {
		if err := execenv.ValidatePath(path); err != nil {
			t.Fatalf("ValidatePath(%q) error = %v", path, err)
		}
	}
	bad := []string{"", "/abs", "a/../b", "a/./b", "a//b", "a/", "a\\b", "a/\x00b", "."}
	for _, path := range bad {
		if err := execenv.ValidatePath(path); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("ValidatePath(%q) error = %v, want ErrInvalid", path, err)
		}
	}
}
