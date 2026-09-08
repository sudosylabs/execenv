package remote

import (
	"bytes"
	"crypto/rand"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
)

const activeProjectionUploads = 4
const projectionUploadIdle = 30 * time.Second

type projectionUpload struct {
	owner     *serverConn
	fence     execenv.ProjectionFence
	data      []byte
	expires   time.Time
	finishing bool
}

// The listener owns one bounded pool across authenticated connections. Bodies
// never become projection content until Finish succeeds under the current fence.
type projectionUploads struct {
	mu      sync.Mutex
	entries map[string]*projectionUpload
}

func newProjectionUploads() *projectionUploads {
	return &projectionUploads{entries: make(map[string]*projectionUpload)}
}
func (p *projectionUploads) prune() {
	for id, item := range p.entries {
		if !item.finishing && !time.Now().Before(item.expires) {
			delete(p.entries, id)
		}
	}
}
func (p *projectionUploads) begin(owner *serverConn, fence execenv.ProjectionFence) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if owner.ctx != nil && owner.ctx.Err() != nil {
		return "", execenv.ErrClosed
	}
	p.prune()
	if len(p.entries) >= activeProjectionUploads {
		return "", execenv.ErrCapacity
	}
	id := rand.Text()
	p.entries[id] = &projectionUpload{owner: owner, fence: fence, expires: time.Now().Add(projectionUploadIdle)}
	return id, nil
}
func (p *projectionUploads) current(owner *serverConn, args projectionUploadArgs) (*projectionUpload, error) {
	p.prune()
	item, ok := p.entries[args.UploadID]
	if !ok || item.owner != owner || item.fence != args.Fence || item.finishing {
		return nil, execenv.ErrConflict
	}
	return item, nil
}
func (p *projectionUploads) append(owner *serverConn, args projectionUploadArgs) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	item, err := p.current(owner, args)
	if err != nil {
		return err
	}
	if len(args.Data) == 0 || len(args.Data) > projectionChunkBytes || args.Offset < 0 {
		return execenv.ErrInvalid
	}
	end := args.Offset + int64(len(args.Data))
	if end > execenv.MaxFileBytes || end < args.Offset {
		return execenv.ErrTooLarge
	}
	if args.Offset < int64(len(item.data)) {
		if end <= int64(len(item.data)) && bytes.Equal(item.data[args.Offset:end], args.Data) {
			return nil
		}
		return execenv.ErrConflict
	}
	if args.Offset != int64(len(item.data)) {
		return execenv.ErrConflict
	}
	item.data = append(item.data, args.Data...)
	item.expires = time.Now().Add(projectionUploadIdle)
	return nil
}
func (p *projectionUploads) finish(owner *serverConn, args projectionUploadArgs) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	item, err := p.current(owner, args)
	if err != nil {
		return nil, err
	}
	if len(args.Data) != 0 || args.Offset != int64(len(item.data)) {
		return nil, execenv.ErrConflict
	}
	item.finishing = true
	return item.data, nil
}
func (p *projectionUploads) abort(owner *serverConn, args projectionUploadArgs) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	item, ok := p.entries[args.UploadID]
	if !ok {
		return nil
	}
	if item.owner != owner || item.fence != args.Fence || item.finishing {
		return execenv.ErrConflict
	}
	delete(p.entries, args.UploadID)
	return nil
}
func (p *projectionUploads) remove(owner *serverConn, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if item := p.entries[id]; item != nil && item.owner == owner {
		delete(p.entries, id)
	}
}
func (p *projectionUploads) close(owner *serverConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, item := range p.entries {
		if item.owner == owner && !item.finishing {
			delete(p.entries, id)
		}
	}
}
