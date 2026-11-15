package journal

import (
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
)

func testStore(t *testing.T) *content.Store {
	t.Helper()
	store, err := content.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// testUpload stores body and registers it, as the agent does.
func testUpload(t *testing.T, j *Journal, fence execenv.ProjectionFence, body string) (execenv.ContentReference, error) {
	t.Helper()
	blob, err := j.Receive(strings.NewReader(body))
	if err != nil {
		return execenv.ContentReference{}, err
	}
	return j.Upload(fence, blob)
}
