package execenvtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/archive"
)

// seedArchive is a small tree: src/, src/main.py, and stale.txt, which the
// initialization below leaves out.
func seedArchive(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w, err := archive.NewWriter(&buf, archive.CodecZstd)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { return w.Dir("src") },
		func() error { return w.File("src/main.py", false, 11, strings.NewReader("print('hi')")) },
		func() error { return w.File("stale.txt", false, 5, strings.NewReader("stale")) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), digest
}

func seeded(body string) *execenv.ContentReference {
	sum := sha256.Sum256([]byte(body))
	return &execenv.ContentReference{Size: int64(len(body)), Digest: hex.EncodeToString(sum[:])}
}

func runSeeds(t *testing.T, factory Factory) {
	t.Run("a seed is put once, listed, and refused with another digest", func(t *testing.T) {
		host := factory(t, Config{})
		raw, digest := seedArchive(t)
		seed, err := host.PutSeed(t.Context(), digest, bytes.NewReader(raw))
		if err != nil || seed.Digest != digest || seed.Entries != 3 || seed.Bytes != 16 {
			t.Fatalf("PutSeed() = %+v, %v", seed, err)
		}
		again, err := host.PutSeed(t.Context(), digest, bytes.NewReader(raw))
		if err != nil || again != seed {
			t.Fatalf("PutSeed() again = %+v, %v", again, err)
		}
		other := strings.Repeat("0", 64)
		if _, err := host.PutSeed(t.Context(), other, bytes.NewReader(raw)); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("PutSeed() under another digest = %v, want ErrInvalid", err)
		}
		seeds, err := host.Seeds(t.Context())
		if err != nil || len(seeds) != 1 || seeds[0] != seed {
			t.Fatalf("Seeds() = %+v, %v", seeds, err)
		}
		if err := host.DeleteSeed(t.Context(), digest); err != nil {
			t.Fatal(err)
		}
		if seeds, _ := host.Seeds(t.Context()); len(seeds) != 0 {
			t.Fatalf("Seeds() after delete = %+v", seeds)
		}
	})

	t.Run("a grant starts from its seed and initializes against it", func(t *testing.T) {
		host := factory(t, Config{})
		raw, digest := seedArchive(t)
		if _, err := host.PutSeed(t.Context(), digest, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "unseeded", Image: "default", Seed: strings.Repeat("a", 64)}); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("Ensure() with an unknown seed = %v, want ErrNotFound", err)
		}
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "seeded", Image: "default", Seed: digest})
		if err != nil {
			t.Fatal(err)
		}
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		wrong := execenv.ProjectionInitialization{Fence: fence, MutationID: "wrong", Entries: []execenv.ProjectionEntry{
			{EntryID: "src", Kind: execenv.ProjectionDirectory, Path: "src"},
			{EntryID: "main", Kind: execenv.ProjectionFile, Path: "src/main.py", ContentVersion: "v1", Content: seeded("print('bye')")},
		}}
		_, err = env.InitializeProjection(t.Context(), wrong)
		var missing *execenv.MissingContentError
		if !errors.As(err, &missing) || len(missing.Paths) != 1 || missing.Paths[0] != "src/main.py" {
			t.Fatalf("InitializeProjection() with a body the seed lacks = %v", err)
		}
		added, err := env.UploadProjectionContent(t.Context(), fence, strings.NewReader("new"))
		if err != nil {
			t.Fatal(err)
		}
		right := execenv.ProjectionInitialization{Fence: fence, MutationID: "right", Entries: []execenv.ProjectionEntry{
			{EntryID: "src", Kind: execenv.ProjectionDirectory, Path: "src"},
			{EntryID: "main", Kind: execenv.ProjectionFile, Path: "src/main.py", ContentVersion: "v1", Content: seeded("print('hi')")},
			{EntryID: "added", Kind: execenv.ProjectionFile, Path: "added.txt", ContentVersion: "v1", Content: &added},
		}}
		if _, err := env.InitializeProjection(t.Context(), right); err != nil {
			t.Fatalf("InitializeProjection() against the seed = %v", err)
		}
		page, err := env.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: 0, Limit: 128})
		if err != nil || len(page.Observations) != 0 {
			t.Fatalf("Observe() after a seeded initialization = %+v, %v; the workspace should match it exactly", page, err)
		}
	})
}
