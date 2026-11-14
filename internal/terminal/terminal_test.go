package terminal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// fakeShell is a controllable shell: the test emits output and observes
// input and resizes.
type fakeShell struct {
	mu      sync.Mutex
	cond    *sync.Cond
	out     []byte
	in      []byte
	sizes   []execenv.Window
	closed  bool
	exitErr error
}

func newFakeShell() *fakeShell {
	s := &fakeShell{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *fakeShell) emit(p []byte) {
	s.mu.Lock()
	s.out = append(s.out, p...)
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *fakeShell) exit(err error) {
	s.mu.Lock()
	s.exitErr = err
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *fakeShell) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.out) == 0 && !s.closed && s.exitErr == nil {
		s.cond.Wait()
	}
	if len(s.out) > 0 {
		n := copy(p, s.out)
		s.out = s.out[n:]
		return n, nil
	}
	if s.exitErr != nil {
		return 0, s.exitErr
	}
	return 0, io.EOF
}

func (s *fakeShell) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, execenv.ErrClosed
	}
	s.in = append(s.in, p...)
	return len(p), nil
}

func (s *fakeShell) Resize(_ context.Context, win execenv.Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes = append(s.sizes, win)
	return nil
}

func (s *fakeShell) Close() error {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

func (s *fakeShell) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeShell) input() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.in)
}

// fakeClock fires grace timers only when the test says so.
type fakeClock struct {
	mu      sync.Mutex
	pending []*fakeTimer
}

type fakeTimer struct {
	f       func()
	stopped bool
}

func (c *fakeClock) AfterFunc(_ time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{f: f}
	c.pending = append(c.pending, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// fire runs every live timer and reports how many ran.
func (c *fakeClock) fire() int {
	c.mu.Lock()
	timers := c.pending
	c.pending = nil
	c.mu.Unlock()
	n := 0
	for _, t := range timers {
		c.mu.Lock()
		live := !t.stopped
		t.stopped = true
		c.mu.Unlock()
		if live {
			t.f()
			n++
		}
	}
	return n
}

type harness struct {
	m      *Manager
	clock  *fakeClock
	mu     sync.Mutex
	shells []*fakeShell
}

func newHarness(t *testing.T, history, tail int) *harness {
	t.Helper()
	h := &harness{clock: &fakeClock{}}
	h.m = New(Config{
		Start: func(_ context.Context, win execenv.Window) (Shell, error) {
			s := newFakeShell()
			s.sizes = append(s.sizes, win)
			h.mu.Lock()
			h.shells = append(h.shells, s)
			h.mu.Unlock()
			return s, nil
		},
		History:   history,
		Tail:      tail,
		AfterFunc: h.clock.AfterFunc,
	})
	t.Cleanup(func() { h.m.End(execenv.ErrRevoked) })
	return h
}

func (h *harness) shell(i int) *fakeShell {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shells[i]
}

func (h *harness) starts() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.shells)
}

func attach(t *testing.T, m *Manager, req execenv.AttachRequest) execenv.Terminal {
	t.Helper()
	term, err := m.Attach(t.Context(), req)
	if err != nil {
		t.Fatalf("Attach(%+v) error = %v", req, err)
	}
	return term
}

// waitTotal waits until the pump has moved want bytes into the ring.
func waitTotal(t *testing.T, m *Manager, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		var total uint64
		if m.sess != nil {
			total = m.sess.total
		}
		m.mu.Unlock()
		if total >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("ring never reached %d bytes", want)
}

func readN(t *testing.T, term execenv.Terminal, n int) []byte {
	t.Helper()
	got := make([]byte, n)
	if _, err := io.ReadFull(term, got); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return got
}

func TestNewSessionStartsAtZero(t *testing.T) {
	h := newHarness(t, 64, 8)
	term := attach(t, h.m, execenv.AttachRequest{Window: execenv.Window{Cols: 100, Rows: 30}})
	if term.Start() != 0 || term.Offset() != 0 || term.ID() == "" {
		t.Fatalf("new session Start=%d Offset=%d ID=%q", term.Start(), term.Offset(), term.ID())
	}
	if got := h.shell(0).sizes; len(got) != 1 || got[0] != (execenv.Window{Cols: 100, Rows: 30}) {
		t.Fatalf("start window = %v", got)
	}
	h.shell(0).emit([]byte("hello"))
	if got := readN(t, term, 5); string(got) != "hello" || term.Offset() != 5 {
		t.Fatalf("read %q offset %d", got, term.Offset())
	}
}

func TestResumeExactReplaysFromOffset(t *testing.T) {
	h := newHarness(t, 64, 8)
	first := attach(t, h.m, execenv.AttachRequest{})
	h.shell(0).emit([]byte("0123456789"))
	readN(t, first, 4)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	waitTotal(t, h.m, 10)
	second := attach(t, h.m, execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: first.ID(), Offset: first.Offset()}})
	if second.ID() != first.ID() || second.Start() != 4 {
		t.Fatalf("resume ID=%q Start=%d", second.ID(), second.Start())
	}
	if got := readN(t, second, 6); string(got) != "456789" {
		t.Fatalf("replay = %q", got)
	}
	if h.starts() != 1 {
		t.Fatal("resume started a new shell")
	}
	// Resume resizes but does not nudge.
	if sizes := h.shell(0).sizes; len(sizes) != 2 || sizes[1] != (execenv.Window{Cols: 80, Rows: 24}) {
		t.Fatalf("resume sizes = %v", sizes)
	}
}

func TestResumeAfterEvictionReportsLoss(t *testing.T) {
	h := newHarness(t, 16, 4)
	first := attach(t, h.m, execenv.AttachRequest{})
	_ = first.Close()
	h.shell(0).emit(bytes.Repeat([]byte("a"), 30))
	h.shell(0).emit([]byte("0123456789"))
	waitTotal(t, h.m, 40)
	second := attach(t, h.m, execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: first.ID(), Offset: 0}})
	if second.Start() != 24 {
		t.Fatalf("Start = %d, want 24 (ring start)", second.Start())
	}
	if got := readN(t, second, 16); string(got) != "aaaaaa0123456789" {
		t.Fatalf("ring = %q", got)
	}
	_ = second.Close()
	_, err := h.m.Attach(t.Context(), execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: first.ID(), Offset: 41}})
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("resume beyond output = %v, want ErrInvalid", err)
	}
}

func TestSlowReaderSkipsAhead(t *testing.T) {
	h := newHarness(t, 16, 4)
	term := attach(t, h.m, execenv.AttachRequest{})
	h.shell(0).emit(bytes.Repeat([]byte("x"), 20))
	h.shell(0).emit([]byte("tail"))
	waitTotal(t, h.m, 24)
	// The reader saw nothing yet; the first 8 bytes were overwritten.
	got := readN(t, term, 16)
	if term.Offset() != 24 || !bytes.HasSuffix(got, []byte("tail")) {
		t.Fatalf("read %q, Offset %d", got, term.Offset())
	}
	if term.Start() != 0 {
		t.Fatalf("Start moved to %d", term.Start())
	}
}

func TestResumeUnknownIsNotFound(t *testing.T) {
	h := newHarness(t, 64, 8)
	_, err := h.m.Attach(t.Context(), execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: "gone"}})
	if !errors.Is(err, execenv.ErrNotFound) {
		t.Fatalf("resume with no session = %v", err)
	}
	term := attach(t, h.m, execenv.AttachRequest{})
	_ = term.Close()
	_, err = h.m.Attach(t.Context(), execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: "gone"}})
	if !errors.Is(err, execenv.ErrNotFound) {
		t.Fatalf("resume of another session = %v", err)
	}
}

func TestBusyAndTakeover(t *testing.T) {
	h := newHarness(t, 64, 8)
	first := attach(t, h.m, execenv.AttachRequest{})
	if _, err := h.m.Attach(t.Context(), execenv.AttachRequest{}); !errors.Is(err, execenv.ErrBusy) {
		t.Fatalf("second attach = %v, want ErrBusy", err)
	}
	readErr := make(chan error, 1)
	go func() {
		_, err := first.Read(make([]byte, 8))
		readErr <- err
	}()
	second := attach(t, h.m, execenv.AttachRequest{Takeover: true})
	if second.ID() != first.ID() {
		t.Fatal("takeover started another session")
	}
	if err := <-readErr; !errors.Is(err, execenv.ErrSuperseded) {
		t.Fatalf("superseded Read = %v", err)
	}
	if _, err := first.Write([]byte("x")); !errors.Is(err, execenv.ErrSuperseded) {
		t.Fatalf("superseded Write = %v", err)
	}
	if err := first.Hangup(t.Context()); !errors.Is(err, execenv.ErrSuperseded) {
		t.Fatalf("superseded Hangup = %v", err)
	}
	if _, err := second.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if h.shell(0).input() != "ok" {
		t.Fatalf("shell input = %q", h.shell(0).input())
	}
	// Closing the replaced attachment does not detach the new one.
	_ = first.Close()
	if _, err := h.m.Attach(t.Context(), execenv.AttachRequest{}); !errors.Is(err, execenv.ErrBusy) {
		t.Fatalf("attach after stale close = %v, want ErrBusy", err)
	}
}

func TestPlainJoinReplaysTailAndRedraws(t *testing.T) {
	h := newHarness(t, 64, 8)
	first := attach(t, h.m, execenv.AttachRequest{})
	_ = first.Close()
	h.shell(0).emit([]byte("abcdefghijklmnop"))
	waitTotal(t, h.m, 16)
	win := execenv.Window{Cols: 120, Rows: 40}
	second := attach(t, h.m, execenv.AttachRequest{Window: win})
	if second.Start() != 8 {
		t.Fatalf("tail Start = %d, want 8", second.Start())
	}
	if got := readN(t, second, 8); string(got) != "ijklmnop" {
		t.Fatalf("tail = %q", got)
	}
	sizes := h.shell(0).sizes
	want := []execenv.Window{{Cols: 80, Rows: 24}, {Cols: 119, Rows: 40}, win}
	if len(sizes) != len(want) {
		t.Fatalf("sizes = %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("sizes = %v, want %v", sizes, want)
		}
	}
}

func TestGraceExpiryEndsSession(t *testing.T) {
	h := newHarness(t, 64, 8)
	first := attach(t, h.m, execenv.AttachRequest{})
	if n := h.clock.fire(); n != 0 {
		t.Fatal("grace armed while attached")
	}
	_ = first.Close()
	if _, err := first.Write([]byte("x")); !errors.Is(err, execenv.ErrClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
	if n := h.clock.fire(); n != 1 {
		t.Fatalf("fired %d timers", n)
	}
	if !h.shell(0).isClosed() {
		t.Fatal("grace expiry did not hang up the shell")
	}
	_, err := h.m.Attach(t.Context(), execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: first.ID()}})
	if !errors.Is(err, execenv.ErrNotFound) {
		t.Fatalf("resume after grace = %v", err)
	}
	next := attach(t, h.m, execenv.AttachRequest{})
	if next.ID() == first.ID() || h.starts() != 2 {
		t.Fatal("attach after grace did not start a new shell")
	}
}

func TestReattachCancelsGrace(t *testing.T) {
	h := newHarness(t, 64, 8)
	first := attach(t, h.m, execenv.AttachRequest{})
	_ = first.Close()
	second := attach(t, h.m, execenv.AttachRequest{})
	if n := h.clock.fire(); n != 0 {
		t.Fatal("grace survived a reattach")
	}
	if h.shell(0).isClosed() || second.ID() != first.ID() {
		t.Fatal("reattach lost the shell")
	}
}

func TestFreezeHoldsGraceUntilThaw(t *testing.T) {
	h := newHarness(t, 64, 8)
	first := attach(t, h.m, execenv.AttachRequest{})
	h.m.Freeze()
	if _, err := first.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("frozen Write = %v", err)
	}
	if _, err := h.m.Attach(t.Context(), execenv.AttachRequest{Takeover: true}); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("frozen Attach = %v", err)
	}
	if err := first.Hangup(t.Context()); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("frozen Hangup = %v", err)
	}
	_ = first.Close()
	if n := h.clock.fire(); n != 0 {
		t.Fatal("grace ran while frozen")
	}
	h.m.Thaw()
	second := attach(t, h.m, execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: first.ID(), Offset: first.Offset()}})
	if second.ID() != first.ID() {
		t.Fatal("thaw lost the session")
	}
	_ = second.Close()
	if n := h.clock.fire(); n != 1 {
		t.Fatalf("grace after thaw fired %d", n)
	}
}

func TestHangupEndsSessionNow(t *testing.T) {
	h := newHarness(t, 64, 8)
	first := attach(t, h.m, execenv.AttachRequest{})
	if err := first.Hangup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !h.shell(0).isClosed() {
		t.Fatal("hangup kept the shell")
	}
	if _, err := first.Write([]byte("x")); err == nil {
		t.Fatal("Write after Hangup succeeded")
	}
	_, err := h.m.Attach(t.Context(), execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: first.ID()}})
	if !errors.Is(err, execenv.ErrNotFound) {
		t.Fatalf("resume after hangup = %v", err)
	}
}

func TestShellExitDrainsThenEOF(t *testing.T) {
	h := newHarness(t, 64, 8)
	term := attach(t, h.m, execenv.AttachRequest{})
	h.shell(0).emit([]byte("bye"))
	h.shell(0).exit(io.EOF)
	got, err := io.ReadAll(term)
	if err != nil || string(got) != "bye" {
		t.Fatalf("ReadAll = %q %v", got, err)
	}
	next := attach(t, h.m, execenv.AttachRequest{})
	if next.ID() == term.ID() {
		t.Fatal("exited shell was reused")
	}
}

func TestEndFailsAttachedTerminal(t *testing.T) {
	h := newHarness(t, 64, 8)
	term := attach(t, h.m, execenv.AttachRequest{})
	h.shell(0).emit([]byte("unread"))
	waitTotal(t, h.m, 6)
	h.m.End(execenv.ErrRevoked)
	if _, err := term.Read(make([]byte, 8)); !errors.Is(err, execenv.ErrRevoked) {
		t.Fatalf("Read after End = %v", err)
	}
	if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrRevoked) {
		t.Fatalf("Write after End = %v", err)
	}
	if _, err := h.m.Attach(t.Context(), execenv.AttachRequest{}); !errors.Is(err, execenv.ErrRevoked) {
		t.Fatalf("Attach after End = %v", err)
	}
}

func TestRingWrapsAtArbitraryBoundaries(t *testing.T) {
	h := newHarness(t, 7, 3)
	term := attach(t, h.m, execenv.AttachRequest{})
	var want bytes.Buffer
	for i := range 50 {
		chunk := bytes.Repeat([]byte{byte('a' + i%26)}, i%5+1)
		want.Write(chunk)
		h.shell(0).emit(chunk)
		got := readN(t, term, len(chunk))
		if !bytes.Equal(got, chunk) {
			t.Fatalf("chunk %d = %q, want %q", i, got, chunk)
		}
	}
	if term.Offset() != uint64(want.Len()) {
		t.Fatalf("Offset = %d, want %d", term.Offset(), want.Len())
	}
}
