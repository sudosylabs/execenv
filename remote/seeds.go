package remote

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/wire"
)

// FeatureSeed is a host that keeps seeds.
const FeatureSeed = "seed"

func seedTo(s execenv.Seed) *execenvv1.Seed {
	return &execenvv1.Seed{Digest: s.Digest, Bytes: s.Bytes, Entries: s.Entries}
}

func seedFrom(s *execenvv1.Seed) execenv.Seed {
	return execenv.Seed{Digest: s.GetDigest(), Bytes: s.GetBytes(), Entries: s.GetEntries()}
}

// Host side.

func (h *server) PutSeed(ctx context.Context, stream *connect.ClientStream[execenvv1.PutSeedRequest]) (*connect.Response[execenvv1.Seed], error) {
	if !stream.Receive() || stream.Msg().GetDigest() == "" {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	digest := stream.Msg().GetDigest()
	body := &wire.ChunkReader{Next: func() ([]byte, bool, error) {
		if !stream.Receive() {
			return nil, false, stream.Err()
		}
		return stream.Msg().GetChunk(), true, nil
	}}
	seed, err := h.inner.PutSeed(ctx, digest, body)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(seedTo(seed)), nil
}

func (h *server) ListSeeds(ctx context.Context, _ *connect.Request[execenvv1.ListSeedsRequest]) (*connect.Response[execenvv1.ListSeedsResponse], error) {
	seeds, err := h.inner.Seeds(ctx)
	if err != nil {
		return nil, wire.Error(err)
	}
	out := &execenvv1.ListSeedsResponse{}
	for _, s := range seeds {
		out.Seeds = append(out.Seeds, seedTo(s))
	}
	return connect.NewResponse(out), nil
}

func (h *server) DeleteSeed(ctx context.Context, req *connect.Request[execenvv1.DeleteSeedRequest]) (*connect.Response[execenvv1.DeleteSeedResponse], error) {
	if err := h.inner.DeleteSeed(ctx, req.Msg.GetDigest()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.DeleteSeedResponse{}), nil
}

// Client side.

// PutSeed streams the archive on the content connection. A seed the host
// already holds is answered after the first message.
func (c *Client) PutSeed(ctx context.Context, digest string, archive io.Reader) (execenv.Seed, error) {
	if execenv.ValidateSeedDigest(digest) != nil || archive == nil {
		return execenv.Seed{}, execenv.Error("put_seed", execenv.ErrInvalid)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	stream := c.bulkHost.PutSeed(ctx)
	failed := func() (execenv.Seed, error) {
		_, err := stream.CloseAndReceive()
		if err == nil {
			err = execenv.ErrUnavailable
		}
		return execenv.Seed{}, wire.HostError("put_seed", err)
	}
	if err := stream.Send(&execenvv1.PutSeedRequest{Part: &execenvv1.PutSeedRequest_Digest{Digest: digest}}); err != nil {
		return failed()
	}
	buf := make([]byte, wire.ChunkSize)
	for {
		n, err := archive.Read(buf)
		if n > 0 {
			if serr := stream.Send(&execenvv1.PutSeedRequest{Part: &execenvv1.PutSeedRequest_Chunk{Chunk: buf[:n]}}); serr != nil {
				// The host may have answered early: it already had the seed.
				return closeSeed(stream)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return execenv.Seed{}, execenv.Error("put_seed", err)
		}
	}
	return closeSeed(stream)
}

func closeSeed(stream *connect.ClientStreamForClient[execenvv1.PutSeedRequest, execenvv1.Seed]) (execenv.Seed, error) {
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return execenv.Seed{}, wire.HostError("put_seed", err)
	}
	return seedFrom(resp.Msg), nil
}

func (c *Client) Seeds(ctx context.Context) ([]execenv.Seed, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.host.ListSeeds(ctx, connect.NewRequest(&execenvv1.ListSeedsRequest{}))
	if err != nil {
		return nil, wire.HostError("seeds", err)
	}
	out := make([]execenv.Seed, 0, len(resp.Msg.GetSeeds()))
	for _, s := range resp.Msg.GetSeeds() {
		out = append(out, seedFrom(s))
	}
	return out, nil
}

func (c *Client) DeleteSeed(ctx context.Context, digest string) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.host.DeleteSeed(ctx, connect.NewRequest(&execenvv1.DeleteSeedRequest{Digest: digest}))
	return wire.HostError("delete_seed", err)
}
