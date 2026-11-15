package execenvtest

import (
	"bytes"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
)

// largeBody is well past the 10 MiB a file was once limited to; only the
// grant's disk bounds a body now.
const largeBody = 24 << 20

func runContent(t *testing.T, factory Factory) {
	t.Run("a body larger than memory caps projects whole", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "large")
		body := bytes.Repeat([]byte("0123456789abcdef"), largeBody/16)
		ref, err := env.UploadProjectionContent(t.Context(), fence, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("UploadProjectionContent(24 MiB) = %v", err)
		}
		if ref.Size != largeBody {
			t.Fatalf("reference size = %d, want %d", ref.Size, largeBody)
		}
		initial := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{
			{EntryID: "big", Kind: execenv.ProjectionFile, Path: "big.bin", ContentVersion: "v1", Content: &ref},
		}}
		if _, err := env.InitializeProjection(t.Context(), initial); err != nil {
			t.Fatalf("InitializeProjection(24 MiB file) = %v", err)
		}
		next, err := env.UploadProjectionContent(t.Context(), fence, io.MultiReader(bytes.NewReader(body), bytes.NewReader(body)))
		if err != nil {
			t.Fatal(err)
		}
		apply := execenv.ProjectionApply{Fence: fence, MutationID: "grow", FromWorkspaceCursor: 0, ThroughWorkspaceCursor: 1, Mutations: []execenv.ProjectionMutation{
			{JournalCursor: 1, EntryID: "big", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "big.bin", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &next},
		}}
		if _, err := env.ApplyProjection(t.Context(), apply); err != nil {
			t.Fatalf("ApplyProjection(48 MiB write) = %v", err)
		}
	})
}
