package execenv_test

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func TestValidateSpec(t *testing.T) {
	t.Parallel()
	err := execenv.ValidateSpec(execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatalf("ValidateSpec() error = %v", err)
	}
	err = execenv.ValidateSpec(execenv.Spec{ID: "bad id", Image: "default"})
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("ValidateSpec() error = %v, want ErrInvalid", err)
	}
}

func TestRequireIsolatedRejectsMemory(t *testing.T) {
	t.Parallel()
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	if host.Capabilities().Isolated {
		t.Fatal("memory Capabilities.Isolated = true")
	}
	if err := execenv.RequireIsolated(host); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("RequireIsolated() error = %v, want ErrUnavailable", err)
	}
}

func TestOpErrorPreservesSentinel(t *testing.T) {
	t.Parallel()
	err := execenv.Error("ensure", execenv.ErrCapacity)
	if !errors.Is(err, execenv.ErrCapacity) {
		t.Fatalf("Error() = %v, want ErrCapacity", err)
	}
	if err.Error() == "ping" {
		t.Fatal("error text unexpectedly contains payload")
	}
}

func TestUnusableHost(t *testing.T) {
	t.Parallel()
	host, err := memory.New(memory.Config{
		Images:   []execenv.Image{"default"},
		Unusable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usable {
		t.Fatal("Ready() Usable = true")
	}
	_, err = host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("Ensure() error = %v, want ErrUnavailable", err)
	}
}
