package memory

import (
	"context"
	"io"
	"os"

	"github.com/sudosylabs/execenv"
)

// PutSeed unpacks a seed into the host's seed directory.
func (h *Host) PutSeed(ctx context.Context, digest string, archive io.Reader) (execenv.Seed, error) {
	if err := ctx.Err(); err != nil {
		return execenv.Seed{}, execenv.Error("put_seed", err)
	}
	seed, err := h.seeds.Put(digest, archive)
	if err != nil {
		return execenv.Seed{}, execenv.Error("put_seed", err)
	}
	return seed, nil
}

func (h *Host) Seeds(ctx context.Context) ([]execenv.Seed, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("seeds", err)
	}
	return h.seeds.List()
}

func (h *Host) DeleteSeed(ctx context.Context, digest string) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("delete_seed", err)
	}
	return h.seeds.Delete(digest)
}

// plant copies seed digest's tree into a new grant's workspace.
func (h *Host) plant(digest, workspace string) error {
	if _, err := h.seeds.Get(digest); err != nil {
		return err
	}
	return os.CopyFS(workspace, os.DirFS(h.seeds.Tree(digest)))
}
