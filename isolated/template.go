package isolated

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
)

// Shape is a machine size the operator offers templates in. A grant whose
// memory is exactly a shape's, and whose CPU needs no more virtual CPUs
// than it has, starts from that shape's template of its image instead of
// booting. Its CPU is still throttled to exactly what it reserved.
type Shape struct {
	// Name is the shape's, in the id alphabet.
	Name        string `json:"name"`
	VCPUs       int    `json:"vcpus"`
	MemoryBytes int64  `json:"memory_bytes"`
}

func validShape(s Shape) bool {
	return s.Name != "" && len(s.Name) <= 32 && execenv.ValidateSpec(execenv.Spec{ID: execenv.ID(s.Name), Image: "x"}) == nil &&
		s.VCPUs >= 1 && s.VCPUs <= 32 && s.MemoryBytes >= 128<<20 && s.MemoryBytes%(1<<20) == 0
}

// templateRetry is how long a template that failed, or found no room to
// build, waits before it is tried again.
const templateRetry = time.Minute

// templateBuildTimeout bounds one build.
const templateBuildTimeout = 5 * time.Minute

type templateKey struct {
	image execenv.Image
	shape string
}

type templateRecord struct {
	image  execenv.Image
	shape  Shape
	dir    string
	ready  bool
	reason string
}

// templates builds and serves a host's templates: one per verified image
// and shape, built one at a time in the background.
type templates struct {
	mu      sync.Mutex
	records map[templateKey]*templateRecord
	wake    chan struct{}
	cancel  context.CancelFunc
}

func (h *Host) startTemplates() {
	h.templates = &templates{records: map[templateKey]*templateRecord{}, wake: make(chan struct{}, 1)}
	builder, ok := h.launch.(templateBuilder)
	if !ok || len(h.cfg.Shapes) == 0 {
		return
	}
	for id := range h.images {
		for _, shape := range h.cfg.Shapes {
			dir := filepath.Join(h.cfg.WorkDir, "templates", string(id)+"-"+shape.Name)
			h.templates.records[templateKey{id, shape.Name}] = &templateRecord{image: id, shape: shape, dir: dir, reason: "building"}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.templates.cancel = cancel
	go h.buildTemplates(ctx, builder)
}

// buildTemplates keeps every template built: those already on disk are
// taken as they are; the rest are built in turn, each retried after a
// failure.
func (h *Host) buildTemplates(ctx context.Context, builder templateBuilder) {
	for {
		pending := false
		for _, rec := range h.templates.list() {
			if rec.ready {
				continue
			}
			pending = true
			h.buildOne(ctx, builder, rec)
			if ctx.Err() != nil {
				return
			}
		}
		wait := templateRetry
		if !pending {
			wait = time.Hour
		}
		select {
		case <-ctx.Done():
			return
		case <-h.templates.wake:
		case <-time.After(wait):
		}
	}
}

func (h *Host) buildOne(ctx context.Context, builder templateBuilder, rec templateRecord) {
	record, ok := h.images[rec.image]
	if !ok {
		return
	}
	image, verified := record.verified()
	if !verified {
		h.templates.set(rec.image, rec.shape.Name, false, "image not verified")
		return
	}
	if readTemplate(rec.dir, image, rec.shape) {
		h.templates.set(rec.image, rec.shape.Name, true, "")
		return
	}
	// A build runs a machine: it takes its size from the pool like a grant.
	hold := execenv.ID("template:" + string(rec.image) + ":" + rec.shape.Name)
	if err := h.pool.Reserve(hold, execenv.Resources{CPUMillis: int64(rec.shape.VCPUs) * 1000, MemoryBytes: rec.shape.MemoryBytes, DiskBytes: templateDiskBytes}); err != nil {
		h.templates.set(rec.image, rec.shape.Name, false, "waiting for room to build")
		return
	}
	defer h.pool.Release(hold)
	bctx, cancel := context.WithTimeout(ctx, templateBuildTimeout)
	defer cancel()
	if err := builder.buildTemplate(bctx, image, rec.shape, rec.dir); err != nil {
		h.templates.set(rec.image, rec.shape.Name, false, "build failed; retrying")
		return
	}
	h.templates.set(rec.image, rec.shape.Name, true, "")
}

func (t *templates) list() []templateRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]templateRecord, 0, len(t.records))
	for _, r := range t.records {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].image != out[j].image {
			return out[i].image < out[j].image
		}
		return out[i].shape.Name < out[j].shape.Name
	})
	return out
}

func (t *templates) set(image execenv.Image, shape string, ready bool, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.records[templateKey{image, shape}]; ok {
		r.ready, r.reason = ready, reason
	}
}

func (t *templates) stop() {
	if t != nil && t.cancel != nil {
		t.cancel()
	}
}

// pick is the template spec starts from, if any: its image, exactly its
// memory, and the fewest virtual CPUs that cover its CPU. A grant with a
// network device always boots: templates are saved without one.
func (t *templates) pick(spec execenv.Spec) (templateRecord, bool) {
	if t == nil || spec.Network != execenv.NetworkNone {
		return templateRecord{}, false
	}
	need := int(vcpuCount(spec.Resources.CPUMillis))
	var best *templateRecord
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.records {
		if !r.ready || r.image != spec.Image || r.shape.MemoryBytes != spec.Resources.MemoryBytes || r.shape.VCPUs < need {
			continue
		}
		if best == nil || r.shape.VCPUs < best.shape.VCPUs {
			best = r
		}
	}
	if best == nil {
		return templateRecord{}, false
	}
	return *best, true
}

// status is the report's view of the templates.
func (t *templates) status() []execenv.TemplateStatus {
	if t == nil {
		return nil
	}
	var out []execenv.TemplateStatus
	for _, r := range t.list() {
		out = append(out, execenv.TemplateStatus{Image: r.image, Shape: r.shape.Name, Ready: r.ready, Reason: r.reason})
	}
	return out
}
