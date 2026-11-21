package agent

import (
	"context"
	"errors"
	"net"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/wire"
)

type oversizePortHelper struct{ otherHelper }

func (oversizePortHelper) DialPort(_ context.Context, stream *connect.BidiStream[execenvv1.PortFrame, execenvv1.PortFrame]) error {
	if _, err := stream.Receive(); err != nil {
		return err
	}
	ack := &execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Opened{Opened: true}}
	// This is a valid acknowledgement if decoded without a message bound.
	ack.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 100, protowire.BytesType), make([]byte, wire.MaxPortMessage)))
	return stream.Send(ack)
}

func TestAgentClientRejectsOversizePortFrame(t *testing.T) {
	host, guest := net.Pipe()
	client := NewClient(host)
	defer client.Close()
	go (&server{}).serveLink(t.Context(), guest, oversizePortHelper{})
	conn, err := client.DialPort(t.Context(), 1234)
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, execenv.ErrConnection) {
		t.Fatalf("oversized acknowledgement = %v, want ErrConnection", err)
	}
}
