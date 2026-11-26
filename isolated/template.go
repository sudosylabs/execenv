package isolated

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
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
	// net is a template saved with a network device, for grants that
	// have one.
	net bool
}

// identity is outside the validated caller ID space and also gives the
// complete tuple a filesystem-safe digest for its snapshot directory.
func (k templateKey) identity() execenv.ID {
	return execenv.ID("\x00template:" + string(k.image) + "\x00" + k.shape + "\x00" + strconv.FormatBool(k.net))
}

func (k templateKey) directory(workDir string) string {
	return filepath.Join(workDir, "templates", fmt.Sprintf("v2-%x", sha256.Sum256([]byte(k.identity()))))
}

type templateRecord struct {
	image  execenv.Image
	shape  Shape
	net    bool
	dir    string
	ready  bool
	reason string
}

func (r templateRecord) key() templateKey { return templateKey{r.image, r.shape.Name, r.net} }

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
	// A host that offers network access saves each template a second
	// time with a network device, for the grants that ask for it.
	nets := []bool{false}
	if len(h.cfg.Allow) > 0 {
		nets = append(nets, true)
	}
	for id := range h.images {
		for _, shape := range h.cfg.Shapes {
			for _, net := range nets {
				rec := &templateRecord{image: id, shape: shape, net: net, reason: "building"}
				rec.dir = rec.key().directory(h.cfg.WorkDir)
				h.templates.records[rec.key()] = rec
			}
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
		h.templates.set(rec.key(), false, "image not verified")
		return
	}
	if readTemplate(rec.dir, image, rec.shape, rec.net) {
		h.templates.set(rec.key(), true, "")
		return
	}
	// A build runs a machine: it takes its size from the pool like a grant.
	hold := rec.key().identity()
	if err := h.pool.Reserve(hold, execenv.Resources{CPUMillis: int64(rec.shape.VCPUs) * 1000, MemoryBytes: rec.shape.MemoryBytes, DiskBytes: templateDiskBytes}); err != nil {
		h.templates.set(rec.key(), false, "waiting for room to build")
		return
	}
	defer h.pool.Release(hold)
	var att *netAttach
	if rec.net {
		// A device for the build alone, filtered as a grant's is. The guest
		// has no address on it, so it sends nothing through it.
		var err error
		att, err = h.attach.Setup(hold, append([]string(nil), h.cfg.Allow...))
		if err != nil {
			h.templates.set(rec.key(), false, "build failed; retrying")
			return
		}
		defer att.close()
	}
	bctx, cancel := context.WithTimeout(ctx, templateBuildTimeout)
	defer cancel()
	if err := builder.buildTemplate(bctx, image, rec.shape, att, rec.dir); err != nil {
		h.templates.set(rec.key(), false, "build failed; retrying")
		return
	}
	h.templates.set(rec.key(), true, "")
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
		if out[i].shape.Name != out[j].shape.Name {
			return out[i].shape.Name < out[j].shape.Name
		}
		return !out[i].net && out[j].net
	})
	return out
}

func (t *templates) set(key templateKey, ready bool, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.records[key]; ok {
		r.ready, r.reason = ready, reason
	}
}

func (t *templates) stop() {
	if t != nil && t.cancel != nil {
		t.cancel()
	}
}

// pick is the template spec starts from, if any: its image, a network
// device when it has one, exactly its memory, and the fewest virtual CPUs
// that cover its CPU.
func (t *templates) pick(spec execenv.Spec) (templateRecord, bool) {
	if t == nil {
		return templateRecord{}, false
	}
	net := spec.Network == execenv.NetworkAllowlist
	need := int(vcpuCount(spec.Resources.CPUMillis))
	var best *templateRecord
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.records {
		if !r.ready || r.image != spec.Image || r.net != net || r.shape.MemoryBytes != spec.Resources.MemoryBytes || r.shape.VCPUs < need {
			continue
		}
		if best == nil || r.shape.VCPUs < best.shape.VCPUs ||
			r.shape.VCPUs == best.shape.VCPUs && r.shape.Name < best.shape.Name {
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
		out = append(out, execenv.TemplateStatus{Image: r.image, Shape: r.shape.Name, Network: r.net, Ready: r.ready, Reason: r.reason})
	}
	return out
}
