package wire

import (
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

func TestPortCreditBounds(t *testing.T) {
	flow := newPortFlow(t.Context(), nil)
	credit := func(amount uint32) error {
		return flow.accept(&execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Credit{Credit: amount}})
	}
	frame := func(size int) error {
		return flow.accept(&execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Data{Data: make([]byte, size)}})
	}
	if err := credit(portWindow); err != nil {
		t.Fatal(err)
	}
	if err := credit(1); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("over-credit = %v", err)
	}
	if err := frame(1); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("data before credit = %v", err)
	}
	flow.granted = portWindow
	if err := frame(portWindow + 1); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("oversized data = %v", err)
	}
	if err := frame(portWindow); err != nil {
		t.Fatal(err)
	}
	if err := frame(1); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("data after consuming credit = %v", err)
	}
}
