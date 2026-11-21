package wire

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/portconn"
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
	for sent := 0; sent < portWindow; sent += portconn.ChunkSize {
		if err := frame(portconn.ChunkSize); err != nil {
			t.Fatal(err)
		}
	}
	if err := frame(1); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("data after consuming credit = %v", err)
	}
}

type delayedFrame struct {
	msg *execenvv1.PortFrame
	at  time.Time
}

type delayedPortStream struct {
	ctx     context.Context
	in, out chan delayedFrame
}

func (s delayedPortStream) Send(msg *execenvv1.PortFrame) error {
	select {
	case s.out <- delayedFrame{msg: msg, at: time.Now().Add(25 * time.Millisecond)}:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s delayedPortStream) Receive() (*execenvv1.PortFrame, error) {
	select {
	case next := <-s.in:
		timer := time.NewTimer(time.Until(next.at))
		defer timer.Stop()
		select {
		case <-timer.C:
			return next.msg, nil
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		}
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func TestSlidingPortCreditThroughput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		data, credit := make(chan delayedFrame, 64), make(chan delayedFrame, 64)
		reader := newPortFlow(ctx, delayedPortStream{ctx: ctx, in: data, out: credit})
		writer := newPortFlow(ctx, delayedPortStream{ctx: ctx, in: credit, out: data})
		go func() { _ = reader.receive() }()
		go func() { _ = writer.receive() }()
		const total = 8 * portWindow
		go func() {
			chunk := make([]byte, portconn.ChunkSize)
			for remaining := total; remaining > 0; {
				credit, err := writer.available()
				if err != nil {
					return
				}
				n := min(credit, len(chunk), remaining)
				if err := writer.data(chunk[:n]); err != nil {
					return
				}
				remaining -= n
			}
			_ = writer.closeWrite()
		}()
		start := time.Now()
		buf := make([]byte, 4<<10)
		n, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, portInput{reader}, buf)
		if err != nil || n != total {
			t.Fatalf("read %d/%d bytes: %v", n, total, err)
		}
		// Eight windows at 50 ms RTT ideally take 400 ms. Allow five
		// times that, while rejecting a round trip per 4 KiB.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("sliding-window transfer took %v", elapsed)
		}
	})
}
