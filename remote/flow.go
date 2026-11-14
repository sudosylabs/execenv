package remote

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/sudosylabs/execenv"
)

// serverStream is one attached terminal stream on a connection with both
// of its flow-control windows. Output waits for client credit outside the
// session write lock; while it has none the pump stops reading the
// terminal, whose session history keeps absorbing shell output. Input is
// queued per stream, outside the connection's operation limit, and its
// credit returns as the terminal takes it.
type serverStream struct {
	sc     *serverConn
	id     execenv.ID
	stream uint64
	term   execenv.Terminal

	mu   sync.Mutex
	cond *sync.Cond
	// outCredit is output the client still accepts, at most outWindow.
	outWindow uint64
	outCredit uint64
	// inCredit is input the client may still send, at most inWindow.
	// inUsed is input written to the terminal and not yet granted back.
	inWindow uint64
	inCredit uint64
	inUsed   uint64
	input    [][]byte
	writing  bool
	closed   bool
	// reason, when set, is the end status sent for the stream in place of
	// whatever the terminal reports.
	reason error
}

func newServerStream(sc *serverConn, id execenv.ID, stream uint64, term execenv.Terminal, outWindow uint64) *serverStream {
	s := &serverStream{
		sc:        sc,
		id:        id,
		stream:    stream,
		term:      term,
		outWindow: outWindow,
		outCredit: outWindow,
		inWindow:  ptyWindow,
		inCredit:  ptyWindow,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// shut stops both directions. Queued input is dropped. The first reason
// wins.
func (s *serverStream) shut(reason error) {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.reason = reason
		s.input = nil
		s.cond.Broadcast()
	}
	s.mu.Unlock()
}

// waitOutput blocks until the client has granted credit and returns how
// many bytes, up to limit, the pump may read. Zero means the stream shut.
func (s *serverStream) waitOutput(limit int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.outCredit == 0 && !s.closed {
		s.cond.Wait()
	}
	if s.closed {
		return 0
	}
	return int(min(uint64(limit), s.outCredit))
}

func (s *serverStream) spendOutput(n int) {
	s.mu.Lock()
	s.outCredit -= uint64(n)
	s.mu.Unlock()
}

// grantOutput adds client credit. It reports false when the grant would
// exceed the window: the client returned bytes it was never sent.
func (s *serverStream) grantOutput(n uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return true
	}
	if n > s.outWindow-s.outCredit {
		return false
	}
	s.outCredit += n
	s.cond.Broadcast()
	return true
}

// acceptInput queues one input frame. It reports false when the frame
// exceeds the client's credit. It never blocks the connection's reader.
func (s *serverStream) acceptInput(p []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(p) == 0 {
		return true
	}
	if uint64(len(p)) > s.inCredit {
		return false
	}
	s.inCredit -= uint64(len(p))
	s.input = append(s.input, p)
	if !s.writing {
		s.writing = true
		go s.drainInput()
	}
	return true
}

// drainInput writes queued input in order and returns credit once at
// least half the window has been taken. Input the terminal refuses still
// counts as taken, so the client never waits on a terminal that is gone;
// a terminal superseded or revoked elsewhere ends the stream.
func (s *serverStream) drainInput() {
	for {
		s.mu.Lock()
		if s.closed || len(s.input) == 0 {
			s.writing = false
			s.cond.Broadcast()
			s.mu.Unlock()
			return
		}
		chunk := s.input[0]
		s.input[0] = nil
		s.input = s.input[1:]
		s.mu.Unlock()
		if _, err := s.term.Write(chunk); errors.Is(err, execenv.ErrSuperseded) || errors.Is(err, execenv.ErrRevoked) {
			// The pump may be waiting for credit a stalled reader will not
			// send; end the stream now so the client's Write stops too.
			s.shut(err)
			continue
		}
		s.mu.Lock()
		s.inUsed += uint64(len(chunk))
		var grant uint64
		if s.inUsed >= s.inWindow/2 && !s.closed {
			grant = s.inUsed
			s.inUsed = 0
			s.inCredit += grant
		}
		s.mu.Unlock()
		if grant > 0 {
			_ = s.sc.sess.Send(frame{Seq: grant, Kind: kindCredit, Grant: string(s.id), Stream: s.stream})
		}
	}
}

// waitInputDrained waits until input received before now reached the
// terminal, the stream shut, or ctx ended.
func (s *serverStream) waitInputDrained(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for (s.writing || len(s.input) > 0) && !s.closed && ctx.Err() == nil {
		s.cond.Wait()
	}
}

// pumpOutput forwards output within the client's credit. Each frame's Seq
// is the absolute terminal offset of its first byte, so the client's
// Offset follows any skip on the host.
func (s *serverStream) pumpOutput() {
	defer s.sc.forgetStream(s)
	buf := make([]byte, 32*1024)
	next := s.term.Start()
	for {
		limit := s.waitOutput(len(buf))
		if limit == 0 {
			s.sendEnd(execenv.ErrClosed)
			return
		}
		n, err := s.term.Read(buf[:limit])
		if n > 0 {
			s.spendOutput(n)
			at := next
			if offset := s.term.Offset(); offset >= uint64(n) && offset-uint64(n) > at {
				at = offset - uint64(n)
			}
			next = at + uint64(n)
			if sendErr := s.sc.sess.Send(frame{Seq: at, Kind: kindPty, Grant: string(s.id), Stream: s.stream, Extra: append([]byte(nil), buf[:n]...)}); sendErr != nil {
				return
			}
		}
		if err != nil {
			s.sendEnd(err)
			return
		}
	}
}

func (s *serverStream) sendEnd(err error) {
	s.mu.Lock()
	reason := s.reason
	s.mu.Unlock()
	status := statusOf(err)
	switch {
	case reason != nil:
		status = statusOf(reason)
	case errors.Is(err, io.EOF):
		status = statusEOF
	}
	_ = s.sc.sess.Send(frame{Kind: kindPty, Grant: string(s.id), Stream: s.stream, Status: status})
}

// stream returns the connection's current stream for f, if f names it.
func (sc *serverConn) stream(f frame) *serverStream {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	s := sc.streams[execenv.ID(f.Grant)]
	if s == nil || s.stream != f.Stream {
		return nil
	}
	return s
}

func (sc *serverConn) forgetStream(s *serverStream) {
	sc.mu.Lock()
	if sc.streams[s.id] == s {
		delete(sc.streams, s.id)
	}
	sc.mu.Unlock()
}

// writePty takes one input frame. Frames for a stream that is gone are
// dropped: they were in flight when it ended.
func (sc *serverConn) writePty(f frame) {
	s := sc.stream(f)
	if s == nil {
		return
	}
	if f.Status != "" || !s.acceptInput(f.Extra) {
		sc.failStream(s, execenv.ErrInvalid)
	}
}

func (sc *serverConn) creditPty(f frame) {
	s := sc.stream(f)
	if s == nil {
		return
	}
	if !s.grantOutput(f.Seq) {
		sc.failStream(s, execenv.ErrInvalid)
	}
}

// failStream ends one stream a peer violated and detaches its terminal.
// The connection and its other streams are unaffected. The client learns
// reason from the stream's end frame.
func (sc *serverConn) failStream(s *serverStream, reason error) {
	sc.forgetStream(s)
	s.shut(reason)
	_ = s.term.Close()
}
