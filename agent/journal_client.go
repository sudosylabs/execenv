package agent

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

func (c *Client) JournalEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.journal && !c.closed
}
func (c *Client) journalCall(ctx context.Context, method string, args any) ([]byte, error) {
	if !c.JournalEnabled() {
		return nil, execenv.ErrUnavailable
	}
	if err := c.ioGuard(); err != nil {
		return nil, err
	}
	raw, err := encodeExtra(args)
	if err != nil {
		return nil, err
	}
	return c.call(ctx, method, raw)
}
func (c *Client) UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, reader io.Reader) (execenv.ContentReference, error) {
	if reader == nil || execenv.ValidateProjectionFence(fence) != nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	raw, err := c.journalCall(ctx, methodJournalUploadBegin, journalArgs{Fence: fence})
	if err != nil {
		return execenv.ContentReference{}, err
	}
	var id string
	if decodeExtra(raw, &id) != nil || id == "" {
		return execenv.ContentReference{}, execenv.ErrUnavailable
	}
	complete := false
	defer func() {
		if !complete {
			cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			raw, _ := encodeExtra(journalArgs{Fence: fence, TransferID: id})
			_, _ = c.call(cleanup, methodJournalUploadAbort, raw)
		}
	}()
	sum := sha256.New()
	offset := int64(0)
	buffer := make([]byte, journalChunkBytes)
	empty := 0
	for {
		if err := ctx.Err(); err != nil {
			return execenv.ContentReference{}, err
		}
		n, readErr := reader.Read(buffer)
		if n < 0 || n > len(buffer) {
			return execenv.ContentReference{}, execenv.ErrInvalid
		}
		if n > 0 {
			empty = 0
			if offset+int64(n) > execenv.MaxFileBytes {
				return execenv.ContentReference{}, execenv.ErrTooLarge
			}
			if _, err := c.journalCall(ctx, methodJournalUploadChunk, journalArgs{Fence: fence, TransferID: id, Offset: offset, Data: buffer[:n]}); err != nil {
				return execenv.ContentReference{}, err
			}
			_, _ = sum.Write(buffer[:n])
			offset += int64(n)
		} else {
			empty++
			if empty >= 100 {
				return execenv.ContentReference{}, io.ErrNoProgress
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return execenv.ContentReference{}, readErr
		}
	}
	raw, err = c.journalCall(ctx, methodJournalUploadFinish, journalArgs{Fence: fence, TransferID: id, Offset: offset})
	if err != nil {
		return execenv.ContentReference{}, err
	}
	var reference execenv.ContentReference
	if decodeExtra(raw, &reference) != nil || execenv.ValidateContentReference(reference) != nil || reference.Size != offset || reference.Digest != hex.EncodeToString(sum.Sum(nil)) {
		return execenv.ContentReference{}, execenv.ErrUnavailable
	}
	complete = true
	return reference, nil
}
func (c *Client) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	if err := execenv.ValidateProjectionInitialization(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	return c.journalReceipt(ctx, methodJournalInitialize, request, request.Fence, request.MutationID, request.WorkspaceCursor, 0)
}
func (c *Client) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	if err := execenv.ValidateProjectionApply(request); err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	return c.journalReceipt(ctx, methodJournalApply, request, request.Fence, request.MutationID, request.ThroughWorkspaceCursor, request.ExpectedHostCursor)
}
func (c *Client) journalReceipt(ctx context.Context, method string, args any, fence execenv.ProjectionFence, id string, cursor, hostCursor uint64) (execenv.ProjectionReceipt, error) {
	raw, err := c.journalCall(ctx, method, args)
	if err != nil {
		return execenv.ProjectionReceipt{}, err
	}
	var receipt execenv.ProjectionReceipt
	if decodeExtra(raw, &receipt) != nil || receipt.Fence != fence || receipt.MutationID != id || receipt.AppliedWorkspaceCursor != cursor || receipt.HostCursor != hostCursor {
		return execenv.ProjectionReceipt{}, execenv.ErrUnavailable
	}
	return receipt, nil
}
func (c *Client) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	raw, err := c.journalCall(ctx, methodJournalObserve, journalArgs{Fence: fence, After: after, Limit: limit})
	if err != nil {
		return execenv.ObservationPage{}, err
	}
	var page execenv.ObservationPage
	if len(raw) > execenv.MaxProjectionMetadataBytes || decodeExtra(raw, &page) != nil || page.HostCursor > execenv.MaxProjectionCursor || page.HostCursor < after || page.AcknowledgedHostCursor > after || len(page.Observations) > limit {
		return execenv.ObservationPage{}, execenv.ErrUnavailable
	}
	for i, event := range page.Observations {
		if execenv.ValidateExecutionObservation(event) != nil || event.Fence.ExecutionGrantID != fence.ExecutionGrantID || event.Fence.EnvironmentEpoch != fence.EnvironmentEpoch || event.HostSequence != after+uint64(i)+1 || event.HostSequence > page.HostCursor {
			return execenv.ObservationPage{}, execenv.ErrUnavailable
		}
	}
	return page, nil
}
func (c *Client) AcknowledgeObservations(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	_, err := c.journalCall(ctx, methodJournalAcknowledge, journalArgs{Fence: fence, Through: through})
	return err
}
func (c *Client) OpenObservationContent(ctx context.Context, fence execenv.ProjectionFence, reference execenv.ContentReference) (io.ReadCloser, error) {
	if execenv.ValidateContentReference(reference) != nil {
		return nil, execenv.ErrInvalid
	}
	// Probe even an empty body so an unknown handle cannot masquerade as a
	// valid zero-byte event. Subsequent reads use that exact immutable reference.
	raw, err := c.journalCall(ctx, methodJournalContent, journalArgs{Fence: fence, Reference: &reference, Length: 1})
	if err != nil {
		return nil, err
	}
	var first []byte
	if decodeExtra(raw, &first) != nil || len(first) != int(min(reference.Size, 1)) {
		return nil, execenv.ErrUnavailable
	}
	return &journalReader{client: c, ctx: ctx, fence: fence, reference: reference, sum: sha256.New()}, nil
}

type journalReader struct {
	mu             sync.Mutex
	client         *Client
	ctx            context.Context
	fence          execenv.ProjectionFence
	reference      execenv.ContentReference
	offset         int64
	sum            hash.Hash
	closed, failed bool
}

func (r *journalReader) Read(p []byte) (int, error) {
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
	length := min(len(p), journalChunkBytes)
	raw, err := r.client.journalCall(r.ctx, methodJournalContent, journalArgs{Fence: r.fence, Reference: &r.reference, Offset: r.offset, Length: length})
	if err != nil {
		return 0, err
	}
	var body []byte
	if len(raw) > journalChunkBytes*2 || decodeExtra(raw, &body) != nil || len(body) != int(min(int64(length), r.reference.Size-r.offset)) {
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
func (r *journalReader) Close() error { r.mu.Lock(); defer r.mu.Unlock(); r.closed = true; return nil }

func (c *Client) ConfirmObservationProjection(ctx context.Context, binding execenv.ObservationProjection) error {
	if err := execenv.ValidateObservationProjection(binding); err != nil {
		return err
	}
	_, err := c.journalCall(ctx, methodJournalConfirm, binding)
	return err
}
