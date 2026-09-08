package remote

import (
	"bytes"
	"crypto/rand"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
)

type semanticDownload struct {
	owner     *serverConn
	fence     execenv.ProjectionFence
	reference execenv.ContentReference
	data      []byte
	loading   bool
	expires   time.Time
}
type semanticDownloads struct {
	mu      sync.Mutex
	entries map[string]*semanticDownload
}

func newSemanticDownloads() *semanticDownloads {
	return &semanticDownloads{entries: make(map[string]*semanticDownload)}
}
func (p *semanticDownloads) prune() {
	for id, item := range p.entries {
		if !item.loading && !time.Now().Before(item.expires) {
			delete(p.entries, id)
		}
	}
}
func (p *semanticDownloads) reserve(owner *serverConn, fence execenv.ProjectionFence, reference execenv.ContentReference) (string, error) {
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
	p.entries[id] = &semanticDownload{owner: owner, fence: fence, reference: reference, loading: true}
	return id, nil
}
func (p *semanticDownloads) complete(owner *serverConn, id string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	item := p.entries[id]
	if item == nil || item.owner != owner || !item.loading {
		return execenv.ErrConflict
	}
	if int64(len(data)) != item.reference.Size || len(data) > execenv.MaxFileBytes {
		return execenv.ErrUnavailable
	}
	item.data = data
	item.loading = false
	item.expires = time.Now().Add(projectionUploadIdle)
	return nil
}
func (p *semanticDownloads) chunk(owner *serverConn, args semanticArgs) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	item := p.entries[args.DownloadID]
	if item == nil || item.owner != owner || item.fence != args.Fence || item.loading {
		return nil, execenv.ErrConflict
	}
	if args.Offset < 0 || args.Offset > int64(len(item.data)) || args.Length < 1 || args.Length > projectionChunkBytes {
		return nil, execenv.ErrInvalid
	}
	end := min(args.Offset+int64(args.Length), int64(len(item.data)))
	item.expires = time.Now().Add(projectionUploadIdle)
	return bytes.Clone(item.data[args.Offset:end]), nil
}
func (p *semanticDownloads) remove(owner *serverConn, fence execenv.ProjectionFence, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	item := p.entries[id]
	if item == nil {
		return nil
	}
	if item.owner != owner || item.fence != fence {
		return execenv.ErrConflict
	}
	delete(p.entries, id)
	return nil
}
func (p *semanticDownloads) close(owner *serverConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, item := range p.entries {
		if item.owner == owner {
			delete(p.entries, id)
		}
	}
}
