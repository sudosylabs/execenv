package execenv_test

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

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
