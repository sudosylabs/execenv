package capacity

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestReserveRefusesWhatDoesNotFitAndReleaseReturnsIt(t *testing.T) {
	total := execenv.Resources{CPUMillis: 2000, MemoryBytes: 1 << 30, DiskBytes: 4 << 30}
	pool, err := New(total, execenv.Resources{CPUMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 2 << 30}, execenv.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	grant := pool.Resolve(execenv.Resources{})
	if err := pool.Reserve("a", grant); err != nil {
		t.Fatal(err)
	}
	if err := pool.Reserve("a", grant); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("double reserve = %v", err)
	}
	if err := pool.Reserve("b", grant); err != nil {
		t.Fatal(err)
	}
	err = pool.Reserve("c", grant)
	var short *execenv.CapacityError
	if !errors.As(err, &short) || short.Resource != execenv.ResourceCPU || short.Free != 0 || short.Requested != 1000 {
		t.Fatalf("third reserve = %v", err)
	}
	pool.Release("a")
	pool.Release("a")
	var report execenv.Report
	pool.Fill(&report)
	if report.CPUMillis != (execenv.Capacity{Total: 2000, Reserved: 1000, Free: 1000}) {
		t.Fatalf("cpu = %+v", report.CPUMillis)
	}
}

func TestReserveRefusesMoreThanTheGrantMaximum(t *testing.T) {
	pool, err := New(execenv.DefaultGrant.Times(4), execenv.Resources{}, execenv.Resources{MemoryBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	big := pool.Resolve(execenv.Resources{MemoryBytes: 2 << 30})
	var short *execenv.CapacityError
	if err := pool.Reserve("big", big); !errors.As(err, &short) || short.Resource != execenv.ResourceMemory || short.Free != 1<<30 {
		t.Fatalf("Reserve() = %v", err)
	}
}

func TestNewRefusesInconsistentSizes(t *testing.T) {
	for name, c := range map[string][3]execenv.Resources{
		"no total":            {{}, {}, {}},
		"max above total":     {execenv.DefaultGrant, {}, execenv.DefaultGrant.Times(2)},
		"defaults above max":  {execenv.DefaultGrant.Times(2), execenv.DefaultGrant.Times(2), execenv.DefaultGrant},
		"defaults below mins": {execenv.DefaultGrant, {CPUMillis: 10, MemoryBytes: 512 << 20, DiskBytes: 2 << 30}, {}},
	} {
		if _, err := New(c[0], c[1], c[2]); !errors.Is(err, execenv.ErrInvalid) {
			t.Errorf("%s: New() = %v, want ErrInvalid", name, err)
		}
	}
}
