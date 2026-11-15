package remote

import (
	"context"
	"io"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/wire"
)

// FeatureFiles is a host that serves the plain file API.
const FeatureFiles = "files"

// Host side.

func (h *server) Stat(ctx context.Context, req *connect.Request[execenvv1.StatRequest]) (*connect.Response[execenvv1.FileInfo], error) {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return nil, wire.Error(err)
	}
	info, err := env.Stat(ctx, req.Msg.GetPath(), req.Msg.GetDigest())
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.FileInfoTo(info)), nil
}

func (h *server) List(ctx context.Context, req *connect.Request[execenvv1.ListRequest]) (*connect.Response[execenvv1.ListResponse], error) {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return nil, wire.Error(err)
	}
	entries, err := env.List(ctx, req.Msg.GetDir(), int(req.Msg.GetDepth()), req.Msg.GetDigest())
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.ListResponse{Entries: wire.FileInfosTo(entries)}), nil
}

func (h *server) ReadFile(ctx context.Context, req *connect.Request[execenvv1.ReadFileRequest], stream *connect.ServerStream[execenvv1.ContentChunk]) error {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return wire.Error(err)
	}
	body, err := env.ReadFile(ctx, req.Msg.GetPath(), req.Msg.GetOffset())
	if err != nil {
		return wire.Error(err)
	}
	defer body.Close()
	return wire.Pump(body, func(b []byte) error { return stream.Send(&execenvv1.ContentChunk{Data: b}) })
}

func (h *server) Export(ctx context.Context, req *connect.Request[execenvv1.ExportRequest], stream *connect.ServerStream[execenvv1.ContentChunk]) error {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return wire.Error(err)
	}
	body, err := env.Export(ctx, req.Msg.GetPaths(), req.Msg.GetCodec())
	if err != nil {
		return wire.Error(err)
	}
	defer body.Close()
	return wire.Pump(body, func(b []byte) error { return stream.Send(&execenvv1.ContentChunk{Data: b}) })
}

func chunks[T interface{ GetChunk() []byte }](receive func() bool, msg func() T, failed func() error) *wire.ChunkReader {
	return &wire.ChunkReader{Next: func() ([]byte, bool, error) {
		if !receive() {
			return nil, false, failed()
		}
		return msg().GetChunk(), true, nil
	}}
}

func (h *server) WriteFile(ctx context.Context, stream *connect.ClientStream[execenvv1.WriteFileRequest]) (*connect.Response[execenvv1.FileInfo], error) {
	if !stream.Receive() || stream.Msg().GetHeader() == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	header := stream.Msg().GetHeader()
	env, fence, err := h.fenced(header.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	body := chunks(stream.Receive, stream.Msg, stream.Err)
	info, err := env.WriteFile(ctx, fence, header.GetPath(), body, wire.WriteOptionsFrom(header))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.FileInfoTo(info)), nil
}

func (h *server) GetTransfer(ctx context.Context, req *connect.Request[execenvv1.GetTransferRequest]) (*connect.Response[execenvv1.Transfer], error) {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return nil, wire.Error(err)
	}
	offset, err := env.TransferOffset(ctx, req.Msg.GetTransferId())
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.Transfer{Offset: offset}), nil
}

func (h *server) Import(ctx context.Context, stream *connect.ClientStream[execenvv1.ImportRequest]) (*connect.Response[execenvv1.ImportResponse], error) {
	if !stream.Receive() || stream.Msg().GetHeader() == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	header := stream.Msg().GetHeader()
	env, fence, err := h.fenced(header.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	if err := env.Import(ctx, fence, header.GetDir(), chunks(stream.Receive, stream.Msg, stream.Err)); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.ImportResponse{}), nil
}

func (h *server) MakeDirectory(ctx context.Context, req *connect.Request[execenvv1.MakeDirectoryRequest]) (*connect.Response[execenvv1.MakeDirectoryResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err == nil {
		err = env.MakeDirectory(ctx, fence, req.Msg.GetPath(), req.Msg.GetParents())
	}
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.MakeDirectoryResponse{}), nil
}

func (h *server) Move(ctx context.Context, req *connect.Request[execenvv1.MoveRequest]) (*connect.Response[execenvv1.MoveResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err == nil {
		err = env.Move(ctx, fence, req.Msg.GetFrom(), req.Msg.GetTo())
	}
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.MoveResponse{}), nil
}

func (h *server) Remove(ctx context.Context, req *connect.Request[execenvv1.RemoveRequest]) (*connect.Response[execenvv1.RemoveResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err == nil {
		err = env.Remove(ctx, fence, req.Msg.GetPath(), req.Msg.GetRecursive())
	}
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.RemoveResponse{}), nil
}

// Client side.

func (e *remoteEnv) Stat(ctx context.Context, path string, digest bool) (execenv.FileInfo, error) {
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.files.Stat(ctx, connect.NewRequest(&execenvv1.StatRequest{GrantId: string(e.id), Epoch: e.epoch, Path: path, Digest: digest}))
	if err != nil {
		return execenv.FileInfo{}, wire.HostError("stat", err)
	}
	return wire.FileInfoFrom(resp.Msg), nil
}

func (e *remoteEnv) List(ctx context.Context, dir string, depth int, digest bool) ([]execenv.FileInfo, error) {
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.files.List(ctx, connect.NewRequest(&execenvv1.ListRequest{GrantId: string(e.id), Epoch: e.epoch, Dir: dir, Depth: int32(depth), Digest: digest}))
	if err != nil {
		return nil, wire.HostError("list", err)
	}
	return wire.FileInfosFrom(resp.Msg.GetEntries()), nil
}

func (e *remoteEnv) streamOut(ctx context.Context, op string, open func(context.Context) (*connect.ServerStreamForClient[execenvv1.ContentChunk], error)) (io.ReadCloser, error) {
	sctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.client.ctx, cancel)
	stream, err := open(sctx)
	if err != nil {
		stop()
		cancel()
		return nil, wire.HostError(op, err)
	}
	r := &wire.StreamReader{Stream: stream, Fail: func(err error) error { return wire.HostError(op, err) }, Release: func() { stop(); cancel() }}
	if err := r.Prime(); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (e *remoteEnv) ReadFile(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	return e.streamOut(ctx, "read_file", func(ctx context.Context) (*connect.ServerStreamForClient[execenvv1.ContentChunk], error) {
		return e.client.bulkFiles.ReadFile(ctx, connect.NewRequest(&execenvv1.ReadFileRequest{GrantId: string(e.id), Epoch: e.epoch, Path: path, Offset: offset}))
	})
}

func (e *remoteEnv) Export(ctx context.Context, paths []string, codec string) (io.ReadCloser, error) {
	return e.streamOut(ctx, "export", func(ctx context.Context) (*connect.ServerStreamForClient[execenvv1.ContentChunk], error) {
		return e.client.bulkFiles.Export(ctx, connect.NewRequest(&execenvv1.ExportRequest{GrantId: string(e.id), Epoch: e.epoch, Paths: paths, Codec: codec}))
	})
}

func (e *remoteEnv) WriteFile(ctx context.Context, fence execenv.ProjectionFence, path string, body io.Reader, opts execenv.WriteOptions) (execenv.FileInfo, error) {
	if err := e.guard(fence); err != nil {
		return execenv.FileInfo{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(e.client.ctx, cancel)
	defer stop()
	stream := e.client.bulkFiles.WriteFile(ctx)
	err := stream.Send(&execenvv1.WriteFileRequest{Part: &execenvv1.WriteFileRequest_Header{Header: wire.WriteHeaderTo(fence, path, opts)}})
	sent := err != nil
	if err == nil {
		err = wire.Pump(body, func(b []byte) error {
			if serr := stream.Send(&execenvv1.WriteFileRequest{Part: &execenvv1.WriteFileRequest_Chunk{Chunk: b}}); serr != nil {
				sent = true
				return serr
			}
			return nil
		})
	}
	if err != nil && !sent {
		// The body failed: it must not reach the host as a complete one.
		cancel()
		_, _ = stream.CloseAndReceive()
		return execenv.FileInfo{}, execenv.Error("write_file", err)
	}
	resp, cerr := stream.CloseAndReceive()
	if cerr != nil {
		return execenv.FileInfo{}, wire.HostError("write_file", cerr)
	}
	if err != nil {
		return execenv.FileInfo{}, execenv.Error("write_file", err)
	}
	return wire.FileInfoFrom(resp.Msg), nil
}

func (e *remoteEnv) TransferOffset(ctx context.Context, transferID string) (int64, error) {
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.files.GetTransfer(ctx, connect.NewRequest(&execenvv1.GetTransferRequest{GrantId: string(e.id), Epoch: e.epoch, TransferId: transferID}))
	if err != nil {
		return 0, wire.HostError("transfer_offset", err)
	}
	return resp.Msg.GetOffset(), nil
}

func (e *remoteEnv) Import(ctx context.Context, fence execenv.ProjectionFence, dir string, archive io.Reader) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(e.client.ctx, cancel)
	defer stop()
	stream := e.client.bulkFiles.Import(ctx)
	err := stream.Send(&execenvv1.ImportRequest{Part: &execenvv1.ImportRequest_Header{Header: &execenvv1.ImportHeader{Fence: wire.FenceTo(fence), Dir: dir}}})
	if err == nil {
		err = wire.Pump(archive, func(b []byte) error {
			return stream.Send(&execenvv1.ImportRequest{Part: &execenvv1.ImportRequest_Chunk{Chunk: b}})
		})
	}
	if _, cerr := stream.CloseAndReceive(); cerr != nil {
		return wire.HostError("import", cerr)
	}
	if err != nil {
		return execenv.Error("import", err)
	}
	return nil
}

func (e *remoteEnv) MakeDirectory(ctx context.Context, fence execenv.ProjectionFence, path string, parents bool) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.files.MakeDirectory(ctx, connect.NewRequest(&execenvv1.MakeDirectoryRequest{Fence: wire.FenceTo(fence), Path: path, Parents: parents}))
	return wire.HostError("make_directory", err)
}

func (e *remoteEnv) Move(ctx context.Context, fence execenv.ProjectionFence, from, to string) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.files.Move(ctx, connect.NewRequest(&execenvv1.MoveRequest{Fence: wire.FenceTo(fence), From: from, To: to}))
	return wire.HostError("move", err)
}

func (e *remoteEnv) Remove(ctx context.Context, fence execenv.ProjectionFence, path string, recursive bool) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.files.Remove(ctx, connect.NewRequest(&execenvv1.RemoveRequest{Fence: wire.FenceTo(fence), Path: path, Recursive: recursive}))
	return wire.HostError("remove", err)
}
