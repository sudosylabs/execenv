package execenv

import "fmt"

// Resources is what one grant is given, or a host's totals. A grant's
// Resources are reserved from the host for the grant's whole life.
type Resources struct {
	// CPUMillis is CPU time per second: 1000 is one full CPU. The guest
	// sees enough virtual CPUs to use it, and is throttled to it.
	CPUMillis int64
	// MemoryBytes is the guest's memory. It is never overcommitted.
	MemoryBytes int64
	// DiskBytes is the grant's workspace disk: the projected tree and the
	// shell's scratch home share it.
	DiskBytes int64
}

// DefaultGrant is a grant's size when neither its Spec nor its host says
// otherwise.
var DefaultGrant = Resources{CPUMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 2 << 30}

// Times is r scaled by n, for example the capacity n grants of size r need.
func (r Resources) Times(n int64) Resources {
	return Resources{CPUMillis: r.CPUMillis * n, MemoryBytes: r.MemoryBytes * n, DiskBytes: r.DiskBytes * n}
}

// Bounds on what one grant may ask for, whatever the host offers.
const (
	MinGrantCPUMillis   = 100
	MinGrantMemoryBytes = 128 << 20
	MinGrantDiskBytes   = 64 << 20
)

// Capacity is one resource of a host. Free is Total less Reserved; it is
// never negative.
type Capacity struct {
	Total    int64
	Reserved int64
	Free     int64
}

// Resource names the resource a CapacityError is about.
type Resource string

const (
	ResourceCPU    Resource = "cpu"
	ResourceMemory Resource = "memory"
	ResourceDisk   Resource = "disk"
)

// CapacityError is the ErrCapacity a host returns when a grant does not
// fit: which resource is short, how much the grant asked for and how much
// is free. errors.Is(err, ErrCapacity) holds.
type CapacityError struct {
	Resource  Resource
	Requested int64
	Free      int64
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("execenv: no capacity: %s requested %d, free %d", e.Resource, e.Requested, e.Free)
}

func (e *CapacityError) Is(target error) bool { return target == ErrCapacity }

// TokenBucket is a rate with a burst: Rate per second, and Burst once
// before the rate applies. A zero Rate is no limit.
type TokenBucket struct {
	Rate  int64
	Burst int64
}

// Limits throttle a grant's devices so one grant cannot starve the host.
// They are changed with Env.UpdateLimits without restarting the grant.
type Limits struct {
	// DiskOps and DiskBytes limit the workspace disk: operations and bytes
	// per second.
	DiskOps   TokenBucket
	DiskBytes TokenBucket
	// NetworkBytes limits the grant's network device in each direction. It
	// has no effect on a grant with NetworkNone.
	NetworkBytes TokenBucket
	// PortBytes limits all of a grant's port connections together, with
	// an independent bucket for each direction.
	PortBytes TokenBucket
}

// IdleAction is what a host does with a grant that has been idle for its
// IdleTimeout.
type IdleAction string

const (
	// IdleFreeze freezes the grant, as a fenced frozen control would. The
	// caller thaws it.
	IdleFreeze IdleAction = "freeze"
	// IdleRevoke revokes the grant.
	IdleRevoke IdleAction = "revoke"
	// IdleSuspend freezes the grant as IdleFreeze does, then saves its
	// running machine to the host's disk and releases its CPU and memory;
	// only its disk stays reserved. A grant the caller froze is suspended
	// the same way once it has been idle that long, and keeps the caller's
	// frozen receipt. The caller's thaw restores it where it stopped, its
	// processes still running; while the host has no room for its CPU and
	// memory the thaw is ErrCapacity and it stays suspended. A host that
	// cannot suspend freezes instead.
	IdleSuspend IdleAction = "suspend"
)

// MaxGrantLabels bounds a grant's labels: at most 16 pairs, 512 bytes in
// all.
const (
	MaxGrantLabels     = 16
	MaxGrantLabelBytes = 512
)

// ValidateResources refuses a grant request below the minimums. A zero
// field means the host's default and is valid.
func ValidateResources(r Resources) error {
	if r.CPUMillis < 0 || r.MemoryBytes < 0 || r.DiskBytes < 0 {
		return ErrInvalid
	}
	if r.CPUMillis != 0 && r.CPUMillis < MinGrantCPUMillis ||
		r.MemoryBytes != 0 && r.MemoryBytes < MinGrantMemoryBytes ||
		r.DiskBytes != 0 && r.DiskBytes < MinGrantDiskBytes {
		return ErrInvalid
	}
	return nil
}

// ValidateLimits refuses negative rates or bursts.
func ValidateLimits(l Limits) error {
	for _, b := range []TokenBucket{l.DiskOps, l.DiskBytes, l.NetworkBytes, l.PortBytes} {
		if b.Rate < 0 || b.Burst < 0 {
			return ErrInvalid
		}
	}
	return nil
}

func validateLifetime(spec Spec) error {
	if spec.WallTimeout < 0 || spec.IdleTimeout < 0 {
		return ErrInvalid
	}
	switch spec.IdleAction {
	case "":
		if spec.IdleTimeout != 0 {
			return ErrInvalid
		}
	case IdleFreeze, IdleRevoke, IdleSuspend:
		if spec.IdleTimeout == 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func validateLabels(labels map[string]string) error {
	if len(labels) > MaxGrantLabels {
		return ErrInvalid
	}
	total := 0
	for k, v := range labels {
		if validateName(k) != nil || len(v) > MaxGrantLabelBytes {
			return ErrInvalid
		}
		total += len(k) + len(v)
	}
	if total > MaxGrantLabelBytes {
		return ErrInvalid
	}
	return nil
}

// Resolve fills a grant request's zero fields from defaults.
func (r Resources) Resolve(defaults Resources) Resources {
	if r.CPUMillis == 0 {
		r.CPUMillis = defaults.CPUMillis
	}
	if r.MemoryBytes == 0 {
		r.MemoryBytes = defaults.MemoryBytes
	}
	if r.DiskBytes == 0 {
		r.DiskBytes = defaults.DiskBytes
	}
	return r
}

// SameGrant reports whether two specs name the same grant: everything but
// Limits, which UpdateLimits changes in place.
func SameGrant(a, b Spec) bool {
	if a.ID != b.ID || a.Image != b.Image || a.Network != b.Network || a.Resources != b.Resources || a.Seed != b.Seed ||
		a.WallTimeout != b.WallTimeout || a.IdleTimeout != b.IdleTimeout || a.IdleAction != b.IdleAction ||
		len(a.Labels) != len(b.Labels) {
		return false
	}
	for k, v := range a.Labels {
		if w, ok := b.Labels[k]; !ok || w != v {
			return false
		}
	}
	return true
}
