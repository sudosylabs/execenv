package agent

import (
	"context"
	"io"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/archive"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/wire"
)

// The guest serves the plain file API from its workspace. Changing calls
// bind the fence through the journal and hold the server's lock, so a
// capture never runs half-way through one of them.

func (s *server) fenced(fence *execenvv1.Fence) error {
	if s.files == nil || s.journal == nil {
		return execenv.ErrUnavailable
	}
	if err := s.guard(); err != nil {
		return err
	}
	return s.journal.Bind(wire.FenceFrom(fence))
}

func (s *server) Stat(_ context.Context, req *connect.Request[execenvv1.StatRequest]) (*connect.Response[execenvv1.FileInfo], error) {
	if s.files == nil {
		return nil, wire.Error(execenv.ErrUnavailable)
	}
	info, err := s.files.Stat(req.Msg.GetPath(), req.Msg.GetDigest())
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.FileInfoTo(info)), nil
}

func (s *server) List(_ context.Context, req *connect.Request[execenvv1.ListRequest]) (*connect.Response[execenvv1.ListResponse], error) {
	if s.files == nil {
		return nil, wire.Error(execenv.ErrUnavailable)
	}
	entries, err := s.files.List(req.Msg.GetDir(), int(req.Msg.GetDepth()), req.Msg.GetDigest())
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.ListResponse{Entries: wire.FileInfosTo(entries)}), nil
}

func (s *server) ReadFile(_ context.Context, req *connect.Request[execenvv1.ReadFileRequest], stream *connect.ServerStream[execenvv1.ContentChunk]) error {
	if s.files == nil {
		return wire.Error(execenv.ErrUnavailable)
	}
	body, err := s.files.ReadFile(req.Msg.GetPath(), req.Msg.GetOffset())
	if err != nil {
		return wire.Error(err)
	}
	defer body.Close()
	return wire.Pump(body, func(b []byte) error { return stream.Send(&execenvv1.ContentChunk{Data: b}) })
}

func (s *server) WriteFile(_ context.Context, stream *connect.ClientStream[execenvv1.WriteFileRequest]) (*connect.Response[execenvv1.FileInfo], error) {
	if !stream.Receive() || stream.Msg().GetHeader() == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	h := stream.Msg().GetHeader()
	body := wire.WriteChunks(stream.Receive, stream.Msg, stream.Err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenced(h.GetFence()); err != nil {
		return nil, wire.Error(err)
	}
	info, err := s.files.WriteFile(h.GetPath(), body, wire.WriteOptionsFrom(h))
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.FileInfoTo(info)), nil
}

func (s *server) GetTransfer(_ context.Context, req *connect.Request[execenvv1.GetTransferRequest]) (*connect.Response[execenvv1.Transfer], error) {
	if s.files == nil {
		return nil, wire.Error(execenv.ErrUnavailable)
	}
	s.mu.Lock()
	offset, err := s.files.TransferOffset(req.Msg.GetTransferId())
	s.mu.Unlock()
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.Transfer{Offset: offset}), nil
}

func (s *server) MakeDirectory(_ context.Context, req *connect.Request[execenvv1.MakeDirectoryRequest]) (*connect.Response[execenvv1.MakeDirectoryResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenced(req.Msg.GetFence()); err != nil {
		return nil, wire.Error(err)
	}
	if err := s.files.MakeDirectory(req.Msg.GetPath(), req.Msg.GetParents()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.MakeDirectoryResponse{}), nil
}

func (s *server) Move(_ context.Context, req *connect.Request[execenvv1.MoveRequest]) (*connect.Response[execenvv1.MoveResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenced(req.Msg.GetFence()); err != nil {
		return nil, wire.Error(err)
	}
	if err := s.files.Move(req.Msg.GetFrom(), req.Msg.GetTo()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.MoveResponse{}), nil
}

func (s *server) Remove(_ context.Context, req *connect.Request[execenvv1.RemoveRequest]) (*connect.Response[execenvv1.RemoveResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenced(req.Msg.GetFence()); err != nil {
		return nil, wire.Error(err)
	}
	if err := s.files.Remove(req.Msg.GetPath(), req.Msg.GetRecursive()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.RemoveResponse{}), nil
}

func (s *server) Import(_ context.Context, stream *connect.ClientStream[execenvv1.ImportRequest]) (*connect.Response[execenvv1.ImportResponse], error) {
	if !stream.Receive() || stream.Msg().GetHeader() == nil {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	h := stream.Msg().GetHeader()
	body := wire.ImportChunks(stream.Receive, stream.Msg, stream.Err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenced(h.GetFence()); err != nil {
		return nil, wire.Error(err)
	}
	if err := s.files.Import(h.GetDir(), body); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.ImportResponse{}), nil
}

func (s *server) Export(_ context.Context, req *connect.Request[execenvv1.ExportRequest], stream *connect.ServerStream[execenvv1.ContentChunk]) error {
	if s.files == nil {
		return wire.Error(execenv.ErrUnavailable)
	}
	codec := archive.Codec(req.Msg.GetCodec())
	if codec == "" {
		codec = archive.CodecZstd
	}
	w := &chunkWriter{send: func(b []byte) error { return stream.Send(&execenvv1.ContentChunk{Data: b}) }}
	if err := s.files.Export(req.Msg.GetPaths(), codec, w); err != nil {
		return wire.Error(err)
	}
	return w.flush()
}

// chunkWriter batches writes into ChunkSize messages.
type chunkWriter struct {
	send func([]byte) error
	buf  []byte
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for len(w.buf) >= wire.ChunkSize {
		if err := w.send(w.buf[:wire.ChunkSize]); err != nil {
			return 0, err
		}
		w.buf = w.buf[wire.ChunkSize:]
	}
	return len(p), nil
}

func (w *chunkWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	err := w.send(w.buf)
	w.buf = nil
	return err
}

// Host side of the file API.

func (c *Client) Stat(ctx context.Context, path string, digest bool) (execenv.FileInfo, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.Stat(ctx, connect.NewRequest(&execenvv1.StatRequest{Path: path, Digest: digest}))
	if err != nil {
		return execenv.FileInfo{}, c.failure("stat", err)
	}
	return wire.FileInfoFrom(resp.Msg), nil
}

func (c *Client) List(ctx context.Context, dir string, depth int, digest bool) ([]execenv.FileInfo, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.List(ctx, connect.NewRequest(&execenvv1.ListRequest{Dir: dir, Depth: int32(min(max(1, depth), execenv.MaxListEntries)), Digest: digest}))
	if err != nil {
		return nil, c.failure("list", err)
	}
	return wire.FileInfosFrom(resp.Msg.GetEntries()), nil
}

func (c *Client) ReadFile(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	return c.streamOut(ctx, "read_file", func(ctx context.Context) (*connect.ServerStreamForClient[execenvv1.ContentChunk], error) {
		return c.agent.ReadFile(ctx, connect.NewRequest(&execenvv1.ReadFileRequest{Path: path, Offset: offset}))
	})
}

func (c *Client) Export(ctx context.Context, paths []string, codec string) (io.ReadCloser, error) {
	return c.streamOut(ctx, "export", func(ctx context.Context) (*connect.ServerStreamForClient[execenvv1.ContentChunk], error) {
		return c.agent.Export(ctx, connect.NewRequest(&execenvv1.ExportRequest{Paths: paths, Codec: codec}))
	})
}

func (c *Client) streamOut(ctx context.Context, op string, open func(context.Context) (*connect.ServerStreamForClient[execenvv1.ContentChunk], error)) (io.ReadCloser, error) {
	sctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	stream, err := open(sctx)
	if err != nil {
		stop()
		cancel()
		return nil, c.failure(op, err)
	}
	r := &wire.StreamReader{Stream: stream, Fail: func(err error) error { return c.failure(op, err) }, Release: func() { stop(); cancel() }}
	if err := r.Prime(); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (c *Client) WriteFile(ctx context.Context, fence execenv.ProjectionFence, path string, body io.Reader, opts execenv.WriteOptions) (execenv.FileInfo, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	stream := c.agent.WriteFile(ctx)
	header := &execenvv1.WriteFileRequest{Part: &execenvv1.WriteFileRequest_Header{Header: wire.WriteHeaderTo(fence, path, opts)}}
	err := stream.Send(header)
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
		// The body failed: it must not reach the guest as a complete one.
		// The abort's reply comes once the guest has kept what arrived.
		wire.AbortWrite(stream.Send, func() error { _, err := stream.CloseAndReceive(); return err }, cancel)
		return execenv.FileInfo{}, execenv.Error("write_file", err)
	}
	resp, cerr := stream.CloseAndReceive()
	if cerr != nil {
		return execenv.FileInfo{}, c.failure("write_file", cerr)
	}
	if err != nil {
		return execenv.FileInfo{}, execenv.Error("write_file", err)
	}
	return wire.FileInfoFrom(resp.Msg), nil
}

func (c *Client) TransferOffset(ctx context.Context, transferID string) (int64, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.GetTransfer(ctx, connect.NewRequest(&execenvv1.GetTransferRequest{TransferId: transferID}))
	if err != nil {
		return 0, c.failure("transfer_offset", err)
	}
	return resp.Msg.GetOffset(), nil
}

func (c *Client) MakeDirectory(ctx context.Context, fence execenv.ProjectionFence, path string, parents bool) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.agent.MakeDirectory(ctx, connect.NewRequest(&execenvv1.MakeDirectoryRequest{Fence: wire.FenceTo(fence), Path: path, Parents: parents}))
	return c.failure("make_directory", err)
}

func (c *Client) Move(ctx context.Context, fence execenv.ProjectionFence, from, to string) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.agent.Move(ctx, connect.NewRequest(&execenvv1.MoveRequest{Fence: wire.FenceTo(fence), From: from, To: to}))
	return c.failure("move", err)
}

func (c *Client) Remove(ctx context.Context, fence execenv.ProjectionFence, path string, recursive bool) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.agent.Remove(ctx, connect.NewRequest(&execenvv1.RemoveRequest{Fence: wire.FenceTo(fence), Path: path, Recursive: recursive}))
	return c.failure("remove", err)
}

func (c *Client) Import(ctx context.Context, fence execenv.ProjectionFence, dir string, body io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	stream := c.agent.Import(ctx)
	err := stream.Send(&execenvv1.ImportRequest{Part: &execenvv1.ImportRequest_Header{Header: &execenvv1.ImportHeader{Fence: wire.FenceTo(fence), Dir: dir}}})
	if err == nil {
		err = wire.SendImport(body, stream.Send)
	}
	if _, cerr := stream.CloseAndReceive(); cerr != nil {
		return c.failure("import", cerr)
	}
	if err != nil {
		return execenv.Error("import", err)
	}
	return nil
}
