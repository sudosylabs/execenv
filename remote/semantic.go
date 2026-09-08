package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
)

var _ execenv.SemanticEnv = (*remoteEnv)(nil)

const (
	methodObservationConfirm     = "semantic_confirm"
	methodObserve                = "semantic_observe"
	methodObservationAcknowledge = "semantic_acknowledge"
	methodObservationOpen        = "semantic_content_open"
	methodObservationRead        = "semantic_content_read"
	methodObservationClose       = "semantic_content_close"
)

type semanticArgs struct {
	Fence      execenv.ProjectionFence   `json:"fence"`
	After      uint64                    `json:"after,omitempty"`
	Limit      int                       `json:"limit,omitempty"`
	Through    uint64                    `json:"through,omitempty"`
	Reference  *execenv.ContentReference `json:"reference,omitempty"`
	DownloadID string                    `json:"download_id,omitempty"`
	Offset     int64                     `json:"offset,omitempty"`
	Length     int                       `json:"length,omitempty"`
}

func (e *remoteEnv) semanticCall(ctx context.Context, method string, args semanticArgs) ([]byte, error) {
	if !e.client.Capabilities().SemanticObservations {
		return nil, execenv.ErrUnavailable
	}
	if err := e.projectionGuard(args.Fence); err != nil {
		return nil, err
	}
	extra, err := encodeExtra(args)
	if err != nil {
		return nil, err
	}
	return e.client.call(ctx, method, string(e.id), extra)
}
func (e *remoteEnv) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	raw, err := e.semanticCall(ctx, methodObserve, semanticArgs{Fence: fence, After: after, Limit: limit})
	if err != nil {
		return execenv.ObservationPage{}, err
	}
	var page execenv.ObservationPage
	if len(raw) > execenv.MaxProjectionMetadataBytes || decodeExtra(raw, &page) != nil || page.HostCursor > execenv.MaxProjectionCursor || page.AcknowledgedHostCursor > page.HostCursor || page.AcknowledgedHostCursor > after || page.HostCursor < after || len(page.Observations) > limit {
		return execenv.ObservationPage{}, execenv.ErrUnavailable
	}
	for i, event := range page.Observations {
		if execenv.ValidateExecutionObservation(event) != nil || event.Fence.ExecutionGrantID != e.id || event.Fence.EnvironmentEpoch != e.epoch || event.HostSequence != after+uint64(i)+1 || event.HostSequence > page.HostCursor {
			return execenv.ObservationPage{}, execenv.ErrUnavailable
		}
	}
	return page, nil
}
func (e *remoteEnv) AcknowledgeObservations(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	_, err := e.semanticCall(ctx, methodObservationAcknowledge, semanticArgs{Fence: fence, Through: through})
	return err
}
func (e *remoteEnv) OpenObservationContent(ctx context.Context, fence execenv.ProjectionFence, reference execenv.ContentReference) (io.ReadCloser, error) {
	if execenv.ValidateContentReference(reference) != nil {
		return nil, execenv.ErrInvalid
	}
	raw, err := e.semanticCall(ctx, methodObservationOpen, semanticArgs{Fence: fence, Reference: &reference})
	if err != nil {
		return nil, err
	}
	var id string
	if decodeExtra(raw, &id) != nil || id == "" {
		return nil, execenv.ErrUnavailable
	}
	return &semanticReader{env: e, ctx: ctx, fence: fence, reference: reference, id: id, sum: sha256.New()}, nil
}

type semanticReader struct {
	mu        sync.Mutex
	env       *remoteEnv
	ctx       context.Context
	fence     execenv.ProjectionFence
	reference execenv.ContentReference
	id        string
	offset    int64
	sum       hash.Hash
	closed    bool
	failed    bool
}

func (r *semanticReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, execenv.ErrClosed
	}
	if r.failed {
		return 0, execenv.ErrUnavailable
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset == r.reference.Size {
		if hex.EncodeToString(r.sum.Sum(nil)) != r.reference.Digest {
			r.failed = true
			return 0, execenv.ErrUnavailable
		}
		return 0, io.EOF
	}
	length := min(len(p), projectionChunkBytes)
	raw, err := r.env.semanticCall(r.ctx, methodObservationRead, semanticArgs{Fence: r.fence, DownloadID: r.id, Offset: r.offset, Length: length})
	if err != nil {
		return 0, err
	}
	var body []byte
	if len(raw) > 2*projectionChunkBytes || decodeExtra(raw, &body) != nil || len(body) != int(min(int64(length), r.reference.Size-r.offset)) {
		r.failed = true
		return 0, execenv.ErrUnavailable
	}
	copy(p, body)
	_, _ = r.sum.Write(body)
	r.offset += int64(len(body))
	if r.offset == r.reference.Size && hex.EncodeToString(r.sum.Sum(nil)) != r.reference.Digest {
		r.failed = true
		return len(body), execenv.ErrUnavailable
	}
	return len(body), nil
}
func (r *semanticReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Closing only releases connection-scoped bytes, even after containment.
	extra, err := encodeExtra(semanticArgs{Fence: r.fence, DownloadID: r.id})
	if err != nil {
		return err
	}
	_, err = r.env.client.call(ctx, methodObservationClose, string(r.env.id), extra)
	return err
}
func (sc *serverConn) semantic(ctx context.Context, f frame) ([]byte, error) {
	if len(f.Extra) > execenv.MaxProjectionMetadataBytes {
		return nil, execenv.ErrTooLarge
	}
	if f.Method == methodObservationConfirm {
		var binding execenv.ObservationProjection
		if decodeExtra(f.Extra, &binding) != nil || execenv.ValidateObservationProjection(binding) != nil {
			return nil, execenv.ErrInvalid
		}
		if !sc.inner.Capabilities().SemanticObservations {
			return nil, execenv.ErrUnavailable
		}
		projected, err := sc.projectionEnvironment(ctx, f.Grant, binding.Fence)
		if err != nil {
			return nil, err
		}
		env, ok := projected.(execenv.SemanticEnv)
		if !ok {
			return nil, execenv.ErrUnavailable
		}
		return nil, env.ConfirmObservationProjection(ctx, binding)
	}
	var args semanticArgs
	if decodeExtra(f.Extra, &args) != nil || execenv.ValidateProjectionFence(args.Fence) != nil || string(args.Fence.ExecutionGrantID) != f.Grant {
		return nil, execenv.ErrInvalid
	}
	if sc.downloads == nil {
		return nil, execenv.ErrUnavailable
	}
	if f.Method == methodObservationClose {
		return nil, sc.downloads.remove(sc, args.Fence, args.DownloadID)
	}
	if !sc.inner.Capabilities().SemanticObservations {
		return nil, execenv.ErrUnavailable
	}
	projected, err := sc.projectionEnvironment(ctx, f.Grant, args.Fence)
	if err != nil {
		return nil, err
	}
	env, ok := projected.(execenv.SemanticEnv)
	if !ok {
		return nil, execenv.ErrUnavailable
	}
	switch f.Method {
	case methodObserve:
		page, err := env.Observe(ctx, args.Fence, args.After, args.Limit)
		if err != nil {
			return nil, err
		}
		return encodeExtra(page)
	case methodObservationAcknowledge:
		return nil, env.AcknowledgeObservations(ctx, args.Fence, args.Through)
	case methodObservationOpen:
		if args.Reference == nil || execenv.ValidateContentReference(*args.Reference) != nil {
			return nil, execenv.ErrInvalid
		}
		id, err := sc.downloads.reserve(sc, args.Fence, *args.Reference)
		if err != nil {
			return nil, err
		}
		completed := false
		defer func() {
			if !completed {
				_ = sc.downloads.remove(sc, args.Fence, id)
			}
		}()
		reader, err := env.OpenObservationContent(ctx, args.Fence, *args.Reference)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		body, err := io.ReadAll(io.LimitReader(reader, execenv.MaxFileBytes+1))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != args.Reference.Digest {
			return nil, execenv.ErrUnavailable
		}
		if err := sc.downloads.complete(sc, id, body); err != nil {
			return nil, err
		}
		completed = true
		return encodeExtra(id)
	case methodObservationRead:
		body, err := sc.downloads.chunk(sc, args)
		if err != nil {
			return nil, err
		}
		return encodeExtra(body)
	default:
		return nil, execenv.ErrInvalid
	}
}

func (e *remoteEnv) ConfirmObservationProjection(ctx context.Context, binding execenv.ObservationProjection) error {
	if !e.client.Capabilities().SemanticObservations {
		return execenv.ErrUnavailable
	}
	if err := e.projectionGuard(binding.Fence); err != nil {
		return err
	}
	if err := execenv.ValidateObservationProjection(binding); err != nil {
		return err
	}
	raw, err := encodeExtra(binding)
	if err != nil {
		return err
	}
	_, err = e.client.call(ctx, methodObservationConfirm, string(e.id), raw)
	return err
}
