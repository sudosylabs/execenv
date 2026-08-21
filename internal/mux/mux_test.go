package mux

import (
	"encoding/binary"
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

func TestSessionRejectsOversizedPty(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	session := NewSession(a)
	err := session.Send(Frame{Kind: KindPty, Extra: make([]byte, MaxPtyBytes+1)})
	if !errors.Is(err, execenv.ErrTooLarge) {
		t.Fatalf("Send() error = %v, want ErrTooLarge", err)
	}
}

func TestSessionRejectsOversizedLengthBeforeAllocation(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	session := NewSession(b)
	go func() {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], MaxFrameBytes+1)
		_, _ = a.Write(header[:])
	}()
	if _, err := session.Recv(); !errors.Is(err, execenv.ErrTooLarge) {
		t.Fatalf("Recv() error = %v, want ErrTooLarge", err)
	}
}
