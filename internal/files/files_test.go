package files

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/archive"
)

func TestWorkspaceFileOperations(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "workspace")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := New(home, filepath.Join(dir, "stage"))
	if err != nil {
		t.Fatal(err)
	}
	if err := files.MakeDirectory("parent/child", true); err != nil {
		t.Fatal(err)
	}
	written, err := files.WriteFile("parent/child/file", strings.NewReader("hello"), execenv.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := files.List("parent", 2, true)
	if err != nil || len(listed) != 2 || listed[1] != written {
		t.Fatalf("listed = %+v, %v; file = %+v", listed, err, written)
	}
	r, err := files.ReadFile(written.Path, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(got) != "ello" {
		t.Fatalf("read = %q, %v", got, err)
	}
	var buf bytes.Buffer
	if err := files.Export([]string{"parent"}, archive.CodecNone, &buf); err != nil {
		t.Fatal(err)
	}
	if err := files.Move("parent", "before"); err != nil {
		t.Fatal(err)
	}
	if err := files.Import("", &buf); err != nil {
		t.Fatal(err)
	}
	imported, err := files.Stat(written.Path, true)
	if err != nil || imported.SHA256 != written.SHA256 {
		t.Fatalf("imported = %+v, %v", imported, err)
	}
	if err := files.Remove("before", true); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Stat("before", false); !errors.Is(err, execenv.ErrNotFound) {
		t.Fatalf("removed directory: %v", err)
	}
}

func TestOpenRegularChecksFileIdentity(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	listed, err := root.Lstat("first")
	if err != nil {
		t.Fatal(err)
	}
	file, err := openRegular(root, "second", listed)
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("changed file identity: %v", err)
	}
}
