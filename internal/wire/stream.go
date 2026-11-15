package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

// ChunkSize bounds one message of file content or terminal output.
const ChunkSize = 64 << 10

// ChunkReader turns a stream of chunks into an io.Reader. next returns the
// next chunk, or false and why the stream ended (nil at its clean end).
// ErrAborted is a body its sender gave up on: its source failed.
var ErrAborted = errors.New("execenv: body aborted by its sender")

// WriteChunks reads a WriteFile body from its stream; an abort message
// ends it with ErrAborted.
func WriteChunks(receive func() bool, msg func() *execenvv1.WriteFileRequest, failed func() error) *ChunkReader {
	return &ChunkReader{Next: func() ([]byte, bool, error) {
		if !receive() {
			return nil, false, failed()
		}
		if msg().GetAbort() {
			return nil, false, ErrAborted
		}
		return msg().GetChunk(), true, nil
	}}
}

// AbortWrite ends a WriteFile whose body failed and waits for the far side
// to have kept what it had; without an abort it cancels.
func AbortWrite(send func(*execenvv1.WriteFileRequest) error, wait func() error, cancel func()) {
	if send(&execenvv1.WriteFileRequest{Part: &execenvv1.WriteFileRequest_Abort{Abort: true}}) != nil {
		cancel()
	}
	_ = wait()
}

type ChunkReader struct {
	Next    func() ([]byte, bool, error)
	pending []byte
	err     error
}

func (r *ChunkReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		chunk, ok, err := r.Next()
		if !ok {
			if err == nil {
				err = io.EOF
			}
			r.err = err
			continue
		}
		r.pending = chunk
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Pump sends r in ChunkSize messages until it ends.
func Pump(r io.Reader, send func([]byte) error) error {
	buf := make([]byte, ChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// StreamReader reads a server stream of content chunks as an io.Reader.
type StreamReader struct {
	Stream  *connect.ServerStreamForClient[execenvv1.ContentChunk]
	Fail    func(error) error
	Release func()
	pending []byte
	err     error
}

func (r *StreamReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if !r.Stream.Receive() {
			r.err = io.EOF
			if err := r.Stream.Err(); err != nil {
				r.err = r.Fail(err)
			}
			continue
		}
		r.pending = r.Stream.Msg().GetData()
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Prime surfaces a refusal sent before any content.
func (r *StreamReader) Prime() error {
	if len(r.pending) > 0 || r.err != nil {
		return nil
	}
	if !r.Stream.Receive() {
		r.err = io.EOF
		if err := r.Stream.Err(); err != nil {
			r.err = r.Fail(err)
			return r.err
		}
		return nil
	}
	r.pending = r.Stream.Msg().GetData()
	return nil
}

func (r *StreamReader) Close() error {
	_ = r.Stream.Close()
	if r.Release != nil {
		r.Release()
		r.Release = nil
	}
	return nil
}

// ContentReader reads a content stream that must match its reference. It
// fails at the end, not cleanly, when the size or digest differ.
type ContentReader struct {
	stream  *connect.ServerStreamForClient[execenvv1.ContentChunk]
	ref     execenv.ContentReference
	op      string
	sum     hash.Hash
	size    int64
	pending []byte
	err     error
	release func()
	// Fail maps a stream error to the caller's error. Nil uses HostError.
	Fail func(error) error
}

// NewContentReader wraps stream. release runs once, on Close.
func NewContentReader(stream *connect.ServerStreamForClient[execenvv1.ContentChunk], ref execenv.ContentReference, op string, release func()) *ContentReader {
	return &ContentReader{stream: stream, ref: ref, op: op, sum: sha256.New(), release: release}
}

// Prime receives the first message, so a host's refusal of an unknown or
// retired body is the opener's error rather than the first Read's.
func (r *ContentReader) Prime() error {
	if err := r.fill(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (r *ContentReader) Read(p []byte) (int, error) {
	if err := r.fill(); err != nil {
		return 0, err
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *ContentReader) fill() error {
	for len(r.pending) == 0 {
		if r.err != nil {
			return r.err
		}
		if !r.stream.Receive() {
			if err := r.stream.Err(); err != nil {
				if r.Fail != nil {
					r.err = r.Fail(err)
				} else {
					r.err = HostError(r.op, err)
				}
			} else if r.size != r.ref.Size || hex.EncodeToString(r.sum.Sum(nil)) != r.ref.Digest {
				r.err = execenv.ErrUnavailable
			} else {
				r.err = io.EOF
			}
			continue
		}
		data := r.stream.Msg().GetData()
		r.size += int64(len(data))
		if r.size > r.ref.Size {
			r.err = execenv.ErrUnavailable
			continue
		}
		_, _ = r.sum.Write(data)
		r.pending = data
	}
	return nil
}

// Close ends the stream. A body read only in part is simply abandoned.
func (r *ContentReader) Close() error {
	_ = r.stream.Close()
	if r.release != nil {
		r.release()
		r.release = nil
	}
	return nil
}
