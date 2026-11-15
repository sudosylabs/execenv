package agent

import (
	"context"
	"errors"
	"net"
	"testing"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	guestv1 "github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1"
	"github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1/guestv1connect"
)

// otherHelper answers Hello for another protocol and serves nothing else.
type otherHelper struct {
	guestv1connect.UnimplementedAgentServiceHandler
	protocol uint32
	journal  bool
}

func (h otherHelper) Hello(context.Context, *connect.Request[guestv1.HelloRequest]) (*connect.Response[guestv1.HelloResponse], error) {
	return connect.NewResponse(&guestv1.HelloResponse{Protocol: h.protocol, Journal: h.journal, Processes: true}), nil
}

func verifyAgainst(t *testing.T, helper otherHelper) error {
	t.Helper()
	clientConn, helperConn := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&server{}).serveLink(ctx, helperConn, helper)
	}()
	client := NewClient(clientConn)
	t.Cleanup(func() { _ = client.Close(); cancel(); <-done })
	return client.Verify(t.Context())
}

func TestVerifyRejectsAnotherProtocol(t *testing.T) {
	if err := verifyAgainst(t, otherHelper{protocol: protocol + 1, journal: true}); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("helper on another protocol accepted: %v", err)
	}
}

func TestVerifyRejectsAHelperWithoutTheJournal(t *testing.T) {
	if err := verifyAgainst(t, otherHelper{protocol: protocol}); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("helper without the journal accepted: %v", err)
	}
}
