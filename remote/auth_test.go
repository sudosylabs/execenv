package remote

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/gen/execenv/v1/execenvv1connect"
	"github.com/sudosylabs/execenv/internal/wire"
	"github.com/sudosylabs/execenv/memory"
)

func serveToken(t *testing.T, token string) string {
	t.Helper()
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(1)})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = Serve(ctx, ln, inner, ServerConfig{Security: SecurityInsecureLocal, Token: []byte(token)})
	}()
	return ln.Addr().String()
}

func TestWrongTokenIsRefused(t *testing.T) {
	addr := serveToken(t, "secret")
	_, err := New(Config{Address: addr, Security: SecurityInsecureLocal, Token: []byte("other")})
	if !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("New() error = %v, want ErrUnavailable", err)
	}
}

func TestHelloRefusesAFeatureTheHostDoesNotOffer(t *testing.T) {
	addr := serveToken(t, "secret")
	client, _ := httpClient(Config{Address: addr, Security: SecurityInsecureLocal, Token: []byte("secret")}, interactiveStreamWindow)
	host := execenvv1connect.NewHostServiceClient(client, "http://"+addr)
	_, err := host.Hello(context.Background(), connect.NewRequest(&execenvv1.HelloRequest{RequiredFeatures: []string{FeatureJournal, "teleport"}}))
	if !errors.Is(wire.HostError("hello", err), execenv.ErrFeature) {
		t.Fatalf("Hello() error = %v, want ErrFeature", err)
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) || len(cerr.Details()) == 0 {
		t.Fatal("refusal carries no detail")
	}
	value, _ := cerr.Details()[0].Value()
	if detail, ok := value.(*execenvv1.ErrorDetail); !ok || detail.GetFeature() != "teleport" {
		t.Fatalf("detail = %v, want feature teleport", value)
	}
}

func TestAuthTimeoutClosesSilentConnection(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = Serve(ctx, ln, inner, ServerConfig{
			Security:    SecurityInsecureLocal,
			Token:       []byte("secret"),
			AuthTimeout: 50 * time.Millisecond,
		})
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := conn.Read(one[:]); err == nil {
		t.Fatal("silent unauthenticated connection remained open")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("client deadline expired before the server auth timeout closed the connection")
	}
}
