package mux

import (
	"errors"
	"io"
	"net"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestStatusRoundTrip(t *testing.T) {
	t.Parallel()
	errs := []error{
		execenv.ErrInvalid,
		execenv.ErrUnknownImage,
		execenv.ErrCapacity,
		execenv.ErrConflict,
		execenv.ErrUnavailable,
		execenv.ErrFrozen,
		execenv.ErrRevoked,
		execenv.ErrBusy,
		execenv.ErrClosed,
		execenv.ErrNotFound,
		execenv.ErrLagged,
		execenv.ErrTooLarge,
		execenv.ErrConnection,
		execenv.ErrNetwork,
	}
	for _, err := range errs {
		got := ErrorFromStatus(StatusOf(err))
		if !errors.Is(got, err) {
			t.Fatalf("StatusOf(%v) round-trip = %v", err, got)
		}
	}
	if StatusOf(nil) != "" || ErrorFromStatus("") != nil {
		t.Fatal("nil status not empty")
	}
	if !errors.Is(ErrorFromStatus("nope"), execenv.ErrUnavailable) {
		t.Fatal("unknown status should be unavailable")
	}
}

func TestSessionSendRecv(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	sa, sb := NewSession(a), NewSession(b)
	go func() {
		_ = sa.Send(Frame{Seq: 7, Kind: KindRequest, Method: "open", Extra: []byte("x")})
	}()
	f, err := sb.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if f.Seq != 7 || f.Kind != KindRequest || f.Method != "open" || string(f.Extra) != "x" {
		t.Fatalf("frame = %+v", f)
	}
	_ = sa.Close()
	if _, err := sb.Recv(); err != io.EOF && err != nil {
		t.Fatalf("recv after close = %v", err)
	}
}

func TestEncodeExtra(t *testing.T) {
	t.Parallel()
	raw, err := EncodeExtra(struct{ N int }{N: 3})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ N int }
	if err := DecodeExtra(raw, &out); err != nil || out.N != 3 {
		t.Fatalf("decode = %+v err=%v", out, err)
	}
	if err := DecodeExtra(nil, &out); err != nil {
		t.Fatal(err)
	}
}
