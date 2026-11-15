package guesttree

import (
	"strings"

	"github.com/sudosylabs/execenv/internal/content"
)

// blob names s as the content store would.
func blob(s string) content.Blob {
	b, _ := content.Hash(strings.NewReader(s))
	return b
}
