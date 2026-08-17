package memory_test

import (
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

func TestConformance(t *testing.T) {
	execenvtest.Run(t, func(t *testing.T) execenv.Host {
		t.Helper()
		host, err := memory.New(memory.Config{
			Images: []execenv.Image{"default", "alt"},
			Slots:  2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return host
	})
}
