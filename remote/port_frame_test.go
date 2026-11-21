package remote

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/gen/execenv/v1/execenvv1connect"
	"github.com/sudosylabs/execenv/internal/wire"
)

type oversizePortHost struct {
	execenvv1connect.UnimplementedHostServiceHandler
	execenvv1connect.UnimplementedPortServiceHandler
}

func (oversizePortHost) Hello(context.Context, *connect.Request[execenvv1.HelloRequest]) (*connect.Response[execenvv1.HelloResponse], error) {
	return connect.NewResponse(&execenvv1.HelloResponse{Protocol: protocol, Features: features}), nil
}

func (oversizePortHost) ConnectPort(_ context.Context, stream *connect.BidiStream[execenvv1.PortFrame, execenvv1.PortFrame]) error {
	if _, err := stream.Receive(); err != nil {
		return err
	}
	ack := &execenvv1.PortFrame{Payload: &execenvv1.PortFrame_Opened{Opened: true}}
	ack.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 100, protowire.BytesType), make([]byte, wire.MaxPortMessage)))
	return stream.Send(ack)
}

func TestRemoteClientRejectsOversizePortFrame(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(execenvv1connect.NewHostServiceHandler(oversizePortHost{}))
	mux.Handle(execenvv1connect.NewPortServiceHandler(oversizePortHost{}))
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(listener) }()
	defer srv.Close()
	client, err := New(Config{Address: listener.Addr().String(), Security: SecurityInsecureLocal})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	env := &remoteEnv{client: client, id: "grant", epoch: "epoch"}
	fence := execenv.ProjectionFence{ExecutionGrantID: env.id, EnvironmentEpoch: env.epoch, FenceRevision: 1}
	conn, err := env.ConnectPort(t.Context(), fence, 1234)
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, execenv.ErrConnection) {
		t.Fatalf("oversized acknowledgement = %v, want ErrConnection", err)
	}
}
