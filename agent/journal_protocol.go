package agent

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/sudosylabs/execenv"
)

const (
	methodJournalConfirm      = "journal_confirm"
	methodJournalUploadBegin  = "journal_upload_begin"
	methodJournalUploadChunk  = "journal_upload_chunk"
	methodJournalUploadFinish = "journal_upload_finish"
	methodJournalUploadAbort  = "journal_upload_abort"
	methodJournalInitialize   = "journal_initialize"
	methodJournalApply        = "journal_apply"
	methodJournalObserve      = "journal_observe"
	methodJournalContent      = "journal_content"
	methodJournalAcknowledge  = "journal_acknowledge"
	journalChunkBytes         = 64 << 10
)

type helloResult struct {
	Version int
	Journal bool
}
type journalArgs struct {
	Fence      execenv.ProjectionFence
	TransferID string
	Offset     int64
	Data       []byte
	After      uint64
	Limit      int
	Through    uint64
	Reference  *execenv.ContentReference
	Length     int
}
type journalUpload struct {
	fence   execenv.ProjectionFence
	id      string
	data    []byte
	expires time.Time
}

func (s *server) journalCall(ctx context.Context, f frame) ([]byte, error) {
	if len(f.Extra) > execenv.MaxProjectionMetadataBytes {
		return nil, execenv.ErrTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal == nil {
		return nil, execenv.ErrUnavailable
	}
	if err := s.guard(); err != nil && f.Method != methodJournalUploadAbort {
		return nil, err
	}
	switch f.Method {
	case methodJournalConfirm:
		var binding execenv.ObservationProjection
		if decodeExtra(f.Extra, &binding) != nil {
			return nil, execenv.ErrInvalid
		}
		return nil, s.journal.confirm(binding)
	case methodJournalInitialize:
		var request execenv.ProjectionInitialization
		if decodeExtra(f.Extra, &request) != nil {
			return nil, execenv.ErrInvalid
		}
		if s.term != nil && !s.journal.state.Initialized() {
			return nil, execenv.ErrConflict
		}
		receipt, err := s.journal.initialize(ctx, request)
		if err != nil {
			return nil, err
		}
		return encodeExtra(receipt)
	case methodJournalApply:
		var request execenv.ProjectionApply
		if decodeExtra(f.Extra, &request) != nil {
			return nil, execenv.ErrInvalid
		}
		receipt, err := s.journal.apply(ctx, request)
		if err != nil {
			return nil, err
		}
		return encodeExtra(receipt)
	}
	var args journalArgs
	if decodeExtra(f.Extra, &args) != nil || execenv.ValidateProjectionFence(args.Fence) != nil {
		return nil, execenv.ErrInvalid
	}
	if err := s.journal.bind(args.Fence); err != nil {
		return nil, err
	}
	switch f.Method {
	case methodJournalObserve:
		page, err := s.journal.observe(ctx, args.Fence, args.After, args.Limit)
		if err != nil {
			return nil, err
		}
		return encodeExtra(page)
	case methodJournalAcknowledge:
		return nil, s.journal.acknowledge(args.Fence, args.Through)
	case methodJournalContent:
		if args.Reference == nil {
			return nil, execenv.ErrInvalid
		}
		body, err := s.journal.log.Chunk(*args.Reference, args.Offset, args.Length)
		if err != nil {
			return nil, err
		}
		return encodeExtra(body)
	case methodJournalUploadBegin:
		if s.journalUpload != nil && time.Now().Before(s.journalUpload.expires) {
			return nil, execenv.ErrBusy
		}
		s.journalUpload = &journalUpload{fence: args.Fence, id: rand.Text(), expires: time.Now().Add(30 * time.Second)}
		return encodeExtra(s.journalUpload.id)
	case methodJournalUploadAbort:
		if s.journalUpload != nil && s.journalUpload.id == args.TransferID && s.journalUpload.fence == args.Fence {
			s.journalUpload = nil
		}
		return nil, nil
	}
	upload := s.journalUpload
	if upload == nil || upload.id != args.TransferID || upload.fence != args.Fence || !time.Now().Before(upload.expires) {
		return nil, execenv.ErrConflict
	}
	if args.Offset != int64(len(upload.data)) {
		return nil, execenv.ErrConflict
	}
	switch f.Method {
	case methodJournalUploadChunk:
		if len(args.Data) == 0 || len(args.Data) > journalChunkBytes || len(upload.data)+len(args.Data) > execenv.MaxFileBytes {
			return nil, execenv.ErrTooLarge
		}
		upload.data = append(upload.data, args.Data...)
		upload.expires = time.Now().Add(30 * time.Second)
		return nil, nil
	case methodJournalUploadFinish:
		reference, err := s.journal.upload(args.Fence, upload.data)
		if err != nil {
			return nil, err
		}
		s.journalUpload = nil
		return encodeExtra(reference)
	default:
		return nil, execenv.ErrInvalid
	}
}
