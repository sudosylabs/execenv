// Package capacity reserves a host's CPU, memory and disk for its grants.
//
// A grant starts with its full reservation and keeps its disk while
// suspended. A reservation that does not fit is refused at once. CPU
// totals include the operator's overcommit. Memory is never overcommitted.
package capacity

import (
	"sync"

	"github.com/sudosylabs/execenv"
)

// Pool is one host's capacity.
type Pool struct {
	total    execenv.Resources
	defaults execenv.Resources
	max      execenv.Resources

	mu   sync.Mutex
	held map[execenv.ID]execenv.Resources
	used execenv.Resources
}

// New returns a pool. defaults fill a request's zero fields (zero:
// execenv.DefaultGrant); max bounds one grant (a zero field: the total).
func New(total, defaults, max execenv.Resources) (*Pool, error) {
	if total.CPUMillis <= 0 || total.MemoryBytes <= 0 || total.DiskBytes <= 0 {
		return nil, execenv.ErrInvalid
	}
	if defaults == (execenv.Resources{}) {
		defaults = execenv.DefaultGrant
	}
	max = max.Resolve(total)
	if max.CPUMillis > total.CPUMillis || max.MemoryBytes > total.MemoryBytes || max.DiskBytes > total.DiskBytes {
		return nil, execenv.ErrInvalid
	}
	if defaults.CPUMillis <= 0 || defaults.MemoryBytes <= 0 || defaults.DiskBytes <= 0 ||
		defaults.CPUMillis > max.CPUMillis || defaults.MemoryBytes > max.MemoryBytes || defaults.DiskBytes > max.DiskBytes {
		return nil, execenv.ErrInvalid
	}
	if execenv.ValidateResources(defaults) != nil {
		return nil, execenv.ErrInvalid
	}
	return &Pool{total: total, defaults: defaults, max: max, held: make(map[execenv.ID]execenv.Resources)}, nil
}

// Defaults and Max are what Report shows callers.
func (p *Pool) Defaults() execenv.Resources { return p.defaults }
func (p *Pool) Max() execenv.Resources      { return p.max }

// Resolve fills r's zero fields with the host's defaults.
func (p *Pool) Resolve(r execenv.Resources) execenv.Resources { return r.Resolve(p.defaults) }

// Reserve takes r, already resolved, for id. Reserving for an id that
// already holds a reservation is a programming error and is refused.
func (p *Pool) Reserve(id execenv.ID, r execenv.Resources) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.held[id]; ok {
		return execenv.ErrConflict
	}
	return p.replaceLocked(id, execenv.Resources{}, r)
}

// Replace changes a reservation atomically. A refusal leaves it intact.
func (p *Pool) Replace(id execenv.ID, r execenv.Resources) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	old, ok := p.held[id]
	if !ok {
		return execenv.ErrNotFound
	}
	return p.replaceLocked(id, old, r)
}

func (p *Pool) replaceLocked(id execenv.ID, old, r execenv.Resources) error {
	free := p.freeLocked()
	for _, c := range []struct {
		resource  execenv.Resource
		requested int64
		free      int64
		max       int64
	}{
		{execenv.ResourceCPU, r.CPUMillis, free.CPUMillis + old.CPUMillis, p.max.CPUMillis},
		{execenv.ResourceMemory, r.MemoryBytes, free.MemoryBytes + old.MemoryBytes, p.max.MemoryBytes},
		{execenv.ResourceDisk, r.DiskBytes, free.DiskBytes + old.DiskBytes, p.max.DiskBytes},
	} {
		if c.requested > c.max || c.requested > c.free {
			return &execenv.CapacityError{Resource: c.resource, Requested: c.requested, Free: min(c.free, c.max)}
		}
	}
	p.held[id] = r
	p.used.CPUMillis += r.CPUMillis - old.CPUMillis
	p.used.MemoryBytes += r.MemoryBytes - old.MemoryBytes
	p.used.DiskBytes += r.DiskBytes - old.DiskBytes
	return nil
}

// Release returns id's reservation. Releasing nothing is a no-op.
func (p *Pool) Release(id execenv.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.held[id]
	if !ok {
		return
	}
	delete(p.held, id)
	p.used.CPUMillis -= r.CPUMillis
	p.used.MemoryBytes -= r.MemoryBytes
	p.used.DiskBytes -= r.DiskBytes
}

func (p *Pool) freeLocked() execenv.Resources {
	return execenv.Resources{
		CPUMillis:   max(0, p.total.CPUMillis-p.used.CPUMillis),
		MemoryBytes: max(0, p.total.MemoryBytes-p.used.MemoryBytes),
		DiskBytes:   max(0, p.total.DiskBytes-p.used.DiskBytes),
	}
}

// Fill writes the pool's capacity into a report.
func (p *Pool) Fill(report *execenv.Report) {
	p.mu.Lock()
	defer p.mu.Unlock()
	free := p.freeLocked()
	report.CPUMillis = execenv.Capacity{Total: p.total.CPUMillis, Reserved: p.used.CPUMillis, Free: free.CPUMillis}
	report.MemoryBytes = execenv.Capacity{Total: p.total.MemoryBytes, Reserved: p.used.MemoryBytes, Free: free.MemoryBytes}
	report.DiskBytes = execenv.Capacity{Total: p.total.DiskBytes, Reserved: p.used.DiskBytes, Free: free.DiskBytes}
	report.Defaults = p.defaults
	report.GrantMax = p.max
}
