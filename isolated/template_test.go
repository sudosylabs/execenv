package isolated

import (
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestTemplateNamesKeepTuplesAndCallerIDsApart(t *testing.T) {
	keys := []templateKey{
		{image: "a-b", shape: "c"},
		{image: "a", shape: "b-c"},
		{image: "a", shape: "b-c", net: true},
		{image: "a", shape: "b-c-net"},
	}
	root := t.TempDir()
	dirs := make(map[string]bool)
	for _, key := range keys {
		dir := key.directory(root)
		if dirs[dir] {
			t.Fatalf("directory is shared by multiple templates: %s", dir)
		}
		dirs[dir] = true
		if execenv.ValidateSpec(execenv.Spec{ID: key.identity(), Image: "image"}) == nil {
			t.Fatal("template runtime identity overlaps caller IDs")
		}
	}
}
