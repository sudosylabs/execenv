package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"github.com/sudosylabs/execenv"
)

var _ execenv.ProjectionEnv = (*remoteEnv)(nil)

const projectionChunkBytes = 64 << 10

type projectionUploadArgs struct {
	Fence    execenv.ProjectionFence `json:"fence"`
	UploadID string                  `json:"upload_id,omitempty"`
	Offset   int64                   `json:"offset,omitempty"`
	Data     []byte                  `json:"data,omitempty"`
}

type projectionUploadResult struct {
	UploadID string `json:"upload_id"`
}

func (e *remoteEnv) projectionGuard(fence execenv.ProjectionFence) error {
	if !e.client.Capabilities().JournalProjection {
		return execenv.ErrUnavailable
	}
	if e.revoked.Load() {
		return execenv.ErrRevoked
	}
	if fence.ExecutionGrantID != e.id || fence.EnvironmentEpoch != e.epoch {
		return execenv.ErrConflict
	}
	return execenv.ValidateProjectionFence(fence)
}

func (e *remoteEnv) UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, reader io.Reader) (execenv.ContentReference, error) {
	if err := e.projectionGuard(fence); err != nil {
		return execenv.ContentReference{}, err
	}
	if reader == nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	extra, err := encodeExtra(projectionUploadArgs{Fence: fence})
	if err != nil {
		return execenv.ContentReference{}, err
	}
	out, err := e.client.call(ctx, methodProjectionUploadBegin, string(e.id), extra)
	if err != nil {
		return execenv.ContentReference{}, err
	}
	var begun projectionUploadResult
	if err := decodeExtra(out, &begun); err != nil || begun.UploadID == "" {
		return execenv.ContentReference{}, execenv.ErrUnavailable
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		extra, err := encodeExtra(projectionUploadArgs{Fence: fence, UploadID: begun.UploadID})
		if err == nil {
			_, _ = e.client.call(cleanup, methodProjectionUploadAbort, string(e.id), extra)
		}
	}()
	buffer := make([]byte, projectionChunkBytes)
	sum := sha256.New()
	var offset int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return execenv.ContentReference{}, err
		}
		n, readErr := reader.Read(buffer)
		if n < 0 || n > len(buffer) {
			return execenv.ContentReference{}, execenv.ErrInvalid
		}
		if n > 0 {
			emptyReads = 0
			if offset+int64(n) > execenv.MaxFileBytes {
				return execenv.ContentReference{}, execenv.ErrTooLarge
			}
			extra, err := encodeExtra(projectionUploadArgs{Fence: fence, UploadID: begun.UploadID, Offset: offset, Data: buffer[:n]})
			if err != nil {
				return execenv.ContentReference{}, err
			}
			if _, err := e.client.call(ctx, methodProjectionUploadChunk, string(e.id), extra); err != nil {
				return execenv.ContentReference{}, err
			}
			_, _ = sum.Write(buffer[:n])
			offset += int64(n)
		} else {
			emptyReads++
			if emptyReads >= 100 && readErr == nil {
				return execenv.ContentReference{}, io.ErrNoProgress
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return execenv.ContentReference{}, readErr
		}
	}
	extra, err = encodeExtra(projectionUploadArgs{Fence: fence, UploadID: begun.UploadID, Offset: offset})
	if err != nil {
		return execenv.ContentReference{}, err
	}
	out, err = e.client.call(ctx, methodProjectionUploadFinish, string(e.id), extra)
	if err != nil {
		return execenv.ContentReference{}, err
	}
	var ref execenv.ContentReference
	if err := decodeExtra(out, &ref); err != nil || execenv.ValidateContentReference(ref) != nil || ref.Size != offset || ref.Digest != hex.EncodeToString(sum.Sum(nil)) {
		return execenv.ContentReference{}, execenv.ErrUnavailable
	}
	completed = true
	return ref, nil
}

func (e *remoteEnv) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	if err := e.projectionGuard(request.Fence); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if err := execenv.ValidateProjectionInitialization(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	extra, err := encodeExtra(request)
	if err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	return e.projectionReceipt(ctx, methodProjectionInitialize, extra, request.Fence, request.MutationID, request.WorkspaceCursor, 0)
}
func (e *remoteEnv) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	if err := e.projectionGuard(request.Fence); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	if err := execenv.ValidateProjectionApply(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	extra, err := encodeExtra(request)
	if err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	return e.projectionReceipt(ctx, methodProjectionApply, extra, request.Fence, request.MutationID, request.ThroughWorkspaceCursor, request.ExpectedHostCursor)
}
func (e *remoteEnv) projectionReceipt(ctx context.Context, method string, extra []byte, fence execenv.ProjectionFence, id string, cursor, hostCursor uint64) (execenv.ProjectionReceipt, error) {
	out, err := e.client.call(ctx, method, string(e.id), extra)
	if err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	var receipt execenv.ProjectionReceipt
	if err := decodeExtra(out, &receipt); err != nil || receipt.Fence != fence || receipt.MutationID != id || receipt.AppliedWorkspaceCursor != cursor || receipt.HostCursor != hostCursor {
		return execenv.ProjectionReceipt{}, execenv.ErrUnavailable
	}
	return receipt, nil
}

func (sc *serverConn) projectionEnvironment(ctx context.Context, grant string, fence execenv.ProjectionFence) (execenv.ProjectionEnv, error) {
	if !sc.inner.Capabilities().JournalProjection {
		return nil, execenv.ErrUnavailable
	}
	if string(fence.ExecutionGrantID) != grant {
		return nil, execenv.ErrInvalid
	}
	if execenv.ValidateProjectionFence(fence) != nil {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(grant))
	if err != nil {
		return nil, err
	}
	projected, ok := env.(execenv.ProjectionEnv)
	if !ok {
		return nil, execenv.ErrUnavailable
	}
	if projected.Epoch() != fence.EnvironmentEpoch {
		return nil, execenv.ErrConflict
	}
	status, err := projected.ControlStatus(ctx)
	if err != nil {
		return nil, err
	}
	if status.State == execenv.ControlRevoked {
		return nil, execenv.ErrRevoked
	}
	if status.Fence != fence {
		return nil, execenv.ErrConflict
	}
	if !status.Confirmed || status.State != execenv.ControlRunning {
		return nil, execenv.ErrFrozen
	}
	return projected, nil
}
func (sc *serverConn) initializeProjection(ctx context.Context, f frame) ([]byte, error) {
	if len(f.Extra) > execenv.MaxProjectionInitializationBytes {
		return nil, execenv.ErrTooLarge
	}
	var request execenv.ProjectionInitialization
	if err := decodeExtra(f.Extra, &request); err != nil {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.projectionEnvironment(ctx, f.Grant, request.Fence)
	if err != nil {
		return nil, err
	}
	receipt, err := env.InitializeProjection(ctx, request)
	if err != nil {
		return nil, err
	}
	return encodeExtra(receipt)
}
func (sc *serverConn) applyProjection(ctx context.Context, f frame) ([]byte, error) {
	if len(f.Extra) > execenv.MaxProjectionMetadataBytes {
		return nil, execenv.ErrTooLarge
	}
	var request execenv.ProjectionApply
	if err := decodeExtra(f.Extra, &request); err != nil {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.projectionEnvironment(ctx, f.Grant, request.Fence)
	if err != nil {
		return nil, err
	}
	receipt, err := env.ApplyProjection(ctx, request)
	if err != nil {
		return nil, err
	}
	return encodeExtra(receipt)
}
func (sc *serverConn) projectionUpload(ctx context.Context, f frame) ([]byte, error) {
	if len(f.Extra) > 2*projectionChunkBytes {
		return nil, execenv.ErrTooLarge
	}
	var args projectionUploadArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return nil, execenv.ErrInvalid
	}
	if sc.uploads == nil {
		return nil, execenv.ErrUnavailable
	}
	// Abort also works after containment; it cannot expose or commit bytes.
	if f.Method == methodProjectionUploadAbort {
		if string(args.Fence.ExecutionGrantID) != f.Grant {
			return nil, execenv.ErrInvalid
		}
		return nil, sc.uploads.abort(sc, args)
	}
	env, err := sc.projectionEnvironment(ctx, f.Grant, args.Fence)
	if err != nil {
		return nil, err
	}
	switch f.Method {
	case methodProjectionUploadBegin:
		if args.UploadID != "" || args.Offset != 0 || len(args.Data) != 0 {
			return nil, execenv.ErrInvalid
		}
		id, err := sc.uploads.begin(sc, args.Fence)
		if err != nil {
			return nil, err
		}
		return encodeExtra(projectionUploadResult{UploadID: id})
	case methodProjectionUploadChunk:
		return nil, sc.uploads.append(sc, args)
	case methodProjectionUploadFinish:
		body, err := sc.uploads.finish(sc, args)
		if err != nil {
			return nil, err
		}
		defer sc.uploads.remove(sc, args.UploadID)
		reference, err := env.UploadProjectionContent(ctx, args.Fence, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		return encodeExtra(reference)
	default:
		return nil, execenv.ErrInvalid
	}
}
