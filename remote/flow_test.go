package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/internal/mux"
	"github.com/sudosylabs/execenv/memory"
)

func TestStalledTerminalDelaysNoOtherStreamOrCall(t *testing.T) {
	t.Parallel()
	client, _ := serveFlow(t, newMemory(t))
	envA := ensureFlow(t, client, "grant-a")
	envB := ensureFlow(t, client, "grant-b")
	termA := attachFlow(t, envA, execenv.AttachRequest{})
	termB := attachFlow(t, envB, execenv.AttachRequest{})

	// A's caller reads nothing while its shell prints more than the host
	// keeps. The client buffers exactly the window and no more.
	writeLine(t, termA, "seq 1 250000")
	writeLine(t, termA, `echo flood""done`)
	eventually(t, "A's buffer to fill its window", func() bool { return buffered(termA) == ptyWindow })

	started := time.Now()
	outB := execenvtest.Watch(t, termB)
	writeLine(t, termB, `echo b""side`)
	outB.WaitFor("bside")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := client.Ready(ctx); err != nil {
		t.Fatalf("Ready() behind a stalled terminal: %v", err)
	}
	if _, err := envB.ControlStatus(ctx); err != nil {
		t.Fatalf("ControlStatus() behind a stalled terminal: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("other stream and calls took %v", elapsed)
	}
	// Give the host's history time to wrap past A's cursor.
	time.Sleep(200 * time.Millisecond)
	if got := buffered(termA); got > ptyWindow {
		t.Fatalf("A buffered %d bytes, window is %d", got, ptyWindow)
	}

	// A resumes reading: the window it buffered, then output from the
	// host's oldest retained byte, with Offset showing the jump.
	watchdog := time.AfterFunc(60*time.Second, func() { _ = termA.Close() })
	defer watchdog.Stop()
	start := termA.Offset()
	var seen []byte
	read := 0
	skipped := false
	buf := make([]byte, 32<<10)
	for !bytes.Contains(seen[max(0, len(seen)-len(buf)-16):], []byte("flooddone")) {
		before := termA.Offset()
		n, err := termA.Read(buf)
		if err != nil {
			t.Fatalf("Read() after resuming: %v after %d bytes", err, read)
		}
		if termA.Offset() != before+uint64(n) {
			skipped = true
		}
		read += n
		seen = append(seen, buf[:n]...)
	}
	if !skipped || termA.Offset() <= start+uint64(read) {
		t.Fatalf("no visible skip: read %d bytes from %d to Offset %d", read, start, termA.Offset())
	}
}

func TestWriteWaitsForHostInputCredit(t *testing.T) {
	t.Parallel()
	gate := newGate()
	t.Cleanup(func() { gate.set(true) })
	client, _ := serveFlow(t, stallHost{Host: newMemory(t), gate: gate, grant: "grant-1"})
	env := ensureFlow(t, client, "grant-1")
	term := attachFlow(t, env, execenv.AttachRequest{})
	payload := bytes.Repeat([]byte("x"), ptyWindow+1000)

	done := writeAsync(term, payload)
	expectBlocked(t, done)
	gate.set(true)
	if r := waitWrite(t, done); r.err != nil || r.n != len(payload) {
		t.Fatalf("Write() after credit returned = %d, %v", r.n, r.err)
	}

	// A Write waiting for credit returns once its terminal closes.
	gate.set(false)
	done = writeAsync(term, payload)
	expectBlocked(t, done)
	closed := make(chan error, 1)
	go func() { closed <- term.Close() }()
	if r := waitWrite(t, done); !errors.Is(r.err, execenv.ErrClosed) || r.n >= len(payload) {
		t.Fatalf("blocked Write() after Close = %d, %v, want ErrClosed", r.n, r.err)
	}
	gate.set(true)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}

	// And once a takeover supersedes it.
	gate.set(false)
	second := attachFlow(t, env, execenv.AttachRequest{})
	done = writeAsync(second, payload)
	expectBlocked(t, done)
	attachFlow(t, env, execenv.AttachRequest{Takeover: true})
	if r := waitWrite(t, done); !errors.Is(r.err, execenv.ErrSuperseded) {
		t.Fatalf("blocked Write() after takeover = %d, %v, want ErrSuperseded", r.n, r.err)
	}
	gate.set(true)
}

func TestStalledReaderLearnsOfTakeoverThroughWrite(t *testing.T) {
	t.Parallel()
	first, addr := serveFlow(t, newMemory(t))
	term := attachFlow(t, ensureFlow(t, first, "grant-1"), execenv.AttachRequest{})
	writeLine(t, term, "seq 1 100000")
	eventually(t, "the window to fill", func() bool { return buffered(term) == ptyWindow })

	second, err := New(Config{Address: addr, Security: SecurityInsecureLocal, Token: []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	attachFlow(t, ensureFlow(t, second, "grant-1"), execenv.AttachRequest{Takeover: true})
	// The host pump of the first stream waits for credit it will never
	// get; its input path still notices the takeover.
	eventually(t, "Write to report the takeover", func() bool {
		_, err := term.Write([]byte("x"))
		return errors.Is(err, execenv.ErrSuperseded)
	})
}

func TestPtyInputSurvivesAFullOperationLimit(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	client, _ := serveFlow(t, blockStatusHost{Host: newMemory(t), release: release})
	blocked := ensureFlow(t, client, "grant-1")
	free := ensureFlow(t, client, "grant-2")
	term := attachFlow(t, free, execenv.AttachRequest{})
	out := execenvtest.Watch(t, term)

	errs := make(chan error, maxConnectionOperations)
	for range maxConnectionOperations {
		go func() {
			for {
				// A probe below may hold a slot for a moment; retry until
				// this call is the one holding it.
				_, err := blocked.ControlStatus(context.Background())
				if !errors.Is(err, execenv.ErrBusy) {
					errs <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	eventually(t, "the operation limit to fill", func() bool {
		_, err := client.Ready(t.Context())
		return errors.Is(err, execenv.ErrBusy)
	})
	for i := range 64 {
		writeLine(t, term, "echo line"+string(rune('a'+i%26)))
	}
	writeLine(t, term, `echo still""up`)
	out.WaitFor("stillup")
	once.Do(func() { close(release) })
	for range maxConnectionOperations {
		if err := <-errs; err != nil {
			t.Fatalf("blocked ControlStatus() = %v", err)
		}
	}
	if _, err := client.Ready(t.Context()); err != nil {
		t.Fatalf("Ready() after the limit drained: %v", err)
	}
}

func TestClientOverrunningInputFailsOnlyItsStream(t *testing.T) {
	t.Parallel()
	gate := newGate()
	t.Cleanup(func() { gate.set(true) })
	_, addr := serveFlow(t, stallHost{Host: newMemory(t), gate: gate, grant: "grant-1"})
	peer := dialRaw(t, addr)
	epoch1 := peer.ensure("grant-1")
	epoch2 := peer.ensure("grant-2")
	peer.attach("grant-1", epoch1, 1, ptyWindow)
	peer.attach("grant-2", epoch2, 2, ptyWindow)

	// The stalled terminal takes nothing, so the fifth full frame is past
	// the host's credit.
	chunk := bytes.Repeat([]byte("x"), mux.MaxPtyBytes)
	for range ptyWindow/mux.MaxPtyBytes + 1 {
		peer.send(frame{Kind: kindPty, Grant: "grant-1", Stream: 1, Extra: chunk})
	}
	if status := peer.waitEnd(1); status != "invalid" {
		t.Fatalf("overrun stream ended %q, want invalid", status)
	}
	peer.mustCall(methodReady, "", nil)
	peer.send(frame{Kind: kindPty, Grant: "grant-2", Stream: 2, Extra: []byte("echo other\"\"ok\n")})
	peer.waitOutput(2, "otherok")
	// The host detached the failed stream's terminal.
	peer.attach("grant-1", epoch1, 3, ptyWindow)
}

func TestClientOverGrantingOutputFailsOnlyItsStream(t *testing.T) {
	t.Parallel()
	_, addr := serveFlow(t, newMemory(t))
	peer := dialRaw(t, addr)
	epoch := peer.ensure("grant-1")
	peer.attach("grant-1", epoch, 1, ptyWindow)
	// Nothing was sent, so any credit is past the window.
	peer.send(frame{Seq: 1, Kind: kindCredit, Grant: "grant-1", Stream: 1})
	if status := peer.waitEnd(1); status != "invalid" {
		t.Fatalf("over-granted stream ended %q, want invalid", status)
	}
	peer.mustCall(methodReady, "", nil)
	peer.attach("grant-1", epoch, 2, ptyWindow)
	peer.send(frame{Kind: kindPty, Grant: "grant-1", Stream: 2, Extra: []byte("echo again\"\"ok\n")})
	peer.waitOutput(2, "againok")
}

func TestHostBreakingFlowControlFailsOnlyThatTerminal(t *testing.T) {
	t.Parallel()
	addr, detached := fakeHost(t)
	client, err := New(Config{Address: addr, Security: SecurityInsecureLocal, Token: []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	flood := attachFlow(t, ensureFlow(t, client, "grant-flood"), execenv.AttachRequest{})
	waitDetached(t, detached, "grant-flood")
	if got := buffered(flood); got > ptyWindow {
		t.Fatalf("client buffered %d bytes past its window", got)
	}
	if _, err := io.Copy(io.Discard, flood); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("Read() of an overrun terminal = %v, want ErrInvalid", err)
	}

	credit := attachFlow(t, ensureFlow(t, client, "grant-credit"), execenv.AttachRequest{})
	waitDetached(t, detached, "grant-credit")
	if _, err := credit.Write([]byte("x")); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("Write() after an over-grant = %v, want ErrInvalid", err)
	}

	if _, err := client.Ready(t.Context()); err != nil {
		t.Fatalf("Ready() after failed streams: %v", err)
	}
	ok := attachFlow(t, ensureFlow(t, client, "grant-ok"), execenv.AttachRequest{})
	got := make([]byte, 2)
	if _, err := io.ReadFull(ok, got); err != nil || string(got) != "ok" {
		t.Fatalf("healthy terminal read %q, %v", got, err)
	}
}

func TestCreditFrameShape(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	sess := newSession(a)
	for _, f := range []frame{
		{Kind: kindCredit, Stream: 1},
		{Seq: 1, Kind: kindCredit},
		{Seq: 1, Kind: kindCredit, Stream: 1, Extra: []byte("x")},
		{Seq: 1, Kind: kindCredit, Stream: 1, Status: "closed"},
	} {
		if err := sess.Send(f); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("Send(%+v) = %v, want ErrInvalid", f, err)
		}
	}
}

func newMemory(t *testing.T) execenv.Host {
	t.Helper()
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	return inner
}

func serveFlow(t *testing.T, inner execenv.Host) (*Client, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = Serve(ctx, ln, inner, ServerConfig{Security: SecurityInsecureLocal, Token: []byte("secret")})
	}()
	client, err := New(Config{Address: ln.Addr().String(), Security: SecurityInsecureLocal, Token: []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, ln.Addr().String()
}

func ensureFlow(t *testing.T, host execenv.Host, id execenv.ID) execenv.Env {
	t.Helper()
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: id, Image: "default"})
	if err != nil {
		t.Fatalf("Ensure(%s) error = %v", id, err)
	}
	return env
}

func attachFlow(t *testing.T, env execenv.Env, request execenv.AttachRequest) execenv.Terminal {
	t.Helper()
	term, err := env.Attach(t.Context(), request)
	if err != nil {
		t.Fatalf("Attach(%+v) error = %v", request, err)
	}
	t.Cleanup(func() { _ = term.Close() })
	return term
}

func writeLine(t *testing.T, term execenv.Terminal, line string) {
	t.Helper()
	if _, err := term.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("Write(%q) error = %v", line, err)
	}
}

func buffered(term execenv.Terminal) int {
	rt := term.(*remoteTerminal)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.buffered
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type writeResult struct {
	n   int
	err error
}

func writeAsync(term execenv.Terminal, p []byte) chan writeResult {
	done := make(chan writeResult, 1)
	go func() {
		n, err := term.Write(p)
		done <- writeResult{n: n, err: err}
	}()
	return done
}

func expectBlocked(t *testing.T, done chan writeResult) {
	t.Helper()
	select {
	case r := <-done:
		t.Fatalf("Write() returned %d, %v before the host granted credit", r.n, r.err)
	case <-time.After(300 * time.Millisecond):
	}
}

func waitWrite(t *testing.T, done chan writeResult) writeResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("Write() did not return")
		return writeResult{}
	}
}

// gate stalls a terminal's input while it is shut.
type gate struct {
	mu   sync.Mutex
	cond *sync.Cond
	open bool
}

func newGate() *gate {
	g := &gate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gate) set(open bool) {
	g.mu.Lock()
	g.open = open
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gate) wait() {
	g.mu.Lock()
	for !g.open {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

// stallHost's grant takes input only while its gate is open, and then
// discards it so no echo competes for output credit.
type stallHost struct {
	execenv.Host
	gate  *gate
	grant execenv.ID
}

func (h stallHost) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	env, err := h.Host.Ensure(ctx, spec)
	if err != nil || spec.ID != h.grant {
		return env, err
	}
	return stallEnv{Env: env, gate: h.gate}, nil
}

type stallEnv struct {
	execenv.Env
	gate *gate
}

func (e stallEnv) Attach(ctx context.Context, request execenv.AttachRequest) (execenv.Terminal, error) {
	term, err := e.Env.Attach(ctx, request)
	if err != nil {
		return nil, err
	}
	return stallTerminal{Terminal: term, gate: e.gate}, nil
}

type stallTerminal struct {
	execenv.Terminal
	gate *gate
}

func (t stallTerminal) Write(p []byte) (int, error) {
	t.gate.wait()
	return len(p), nil
}

// blockStatusHost holds grant-1's ControlStatus until release closes.
type blockStatusHost struct {
	execenv.Host
	release chan struct{}
}

func (h blockStatusHost) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	env, err := h.Host.Ensure(ctx, spec)
	if err != nil || spec.ID != "grant-1" {
		return env, err
	}
	return blockStatusEnv{Env: env, release: h.release}, nil
}

type blockStatusEnv struct {
	execenv.Env
	release chan struct{}
}

func (e blockStatusEnv) ControlStatus(ctx context.Context) (execenv.ControlReceipt, error) {
	select {
	case <-ctx.Done():
		return execenv.ControlReceipt{}, ctx.Err()
	case <-e.release:
		return e.Env.ControlStatus(ctx)
	}
}

// rawPeer speaks the wire directly, so a test can break flow control.
type rawPeer struct {
	t       *testing.T
	sess    *mux.Session
	seq     atomic.Uint64
	mu      sync.Mutex
	replies map[uint64]chan frame
	frames  chan frame
}

func dialRaw(t *testing.T, addr string) *rawPeer {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	p := &rawPeer{t: t, sess: newSession(conn), replies: make(map[uint64]chan frame), frames: make(chan frame, 4096)}
	t.Cleanup(func() { _ = p.sess.Close() })
	go p.read()
	extra, err := encodeExtra(authArgs{Protocol: protocolVersion, Token: []byte("secret"), Release: execenv.Release})
	if err != nil {
		t.Fatal(err)
	}
	p.mustCall(methodAuth, "", extra)
	return p
}

func (p *rawPeer) read() {
	for {
		f, err := p.sess.Recv()
		if err != nil {
			return
		}
		if f.Kind == kindResponse {
			p.mu.Lock()
			ch := p.replies[f.Seq]
			delete(p.replies, f.Seq)
			p.mu.Unlock()
			if ch != nil {
				ch <- f
			}
			continue
		}
		select {
		case p.frames <- f:
		default:
		}
	}
}

func (p *rawPeer) send(f frame) {
	p.t.Helper()
	if err := p.sess.Send(f); err != nil {
		p.t.Fatalf("Send(kind %d) error = %v", f.Kind, err)
	}
}

func (p *rawPeer) mustCall(method, grant string, extra []byte) []byte {
	p.t.Helper()
	seq := p.seq.Add(1)
	ch := make(chan frame, 1)
	p.mu.Lock()
	p.replies[seq] = ch
	p.mu.Unlock()
	p.send(frame{Seq: seq, Kind: kindRequest, Method: method, Grant: grant, Extra: extra})
	select {
	case f := <-ch:
		if f.Status != "" {
			p.t.Fatalf("%s(%s) status %q", method, grant, f.Status)
		}
		return f.Extra
	case <-time.After(10 * time.Second):
		p.t.Fatalf("%s(%s) got no reply; connection lost?", method, grant)
		return nil
	}
}

func (p *rawPeer) ensure(id execenv.ID) string {
	p.t.Helper()
	extra, err := encodeExtra(ensureArgs{Spec: execenv.Spec{ID: id, Image: "default"}})
	if err != nil {
		p.t.Fatal(err)
	}
	var result ensureResult
	if err := decodeExtra(p.mustCall(methodEnsure, string(id), extra), &result); err != nil {
		p.t.Fatal(err)
	}
	return result.Epoch
}

func (p *rawPeer) attach(id execenv.ID, epoch string, stream, credit uint64) {
	p.t.Helper()
	body, err := encodeExtra(attachArgs{Stream: stream, Credit: credit})
	if err != nil {
		p.t.Fatal(err)
	}
	extra, err := encodeExtra(epochArgs{Epoch: epoch, Body: body})
	if err != nil {
		p.t.Fatal(err)
	}
	p.mustCall(methodAttach, string(id), extra)
}

// waitEnd returns the end status of stream, discarding other frames.
func (p *rawPeer) waitEnd(stream uint64) string {
	p.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-p.frames:
			if f.Kind == kindPty && f.Stream == stream && f.Status != "" {
				return f.Status
			}
		case <-timeout:
			p.t.Fatalf("stream %d never ended", stream)
			return ""
		}
	}
}

func (p *rawPeer) waitOutput(stream uint64, marker string) {
	p.t.Helper()
	timeout := time.After(10 * time.Second)
	var seen []byte
	for {
		select {
		case f := <-p.frames:
			if f.Kind == kindPty && f.Stream == stream {
				if f.Status != "" {
					p.t.Fatalf("stream %d ended %q before %q", stream, f.Status, marker)
				}
				seen = append(seen, f.Extra...)
				if bytes.Contains(seen, []byte(marker)) {
					return
				}
			}
		case <-timeout:
			p.t.Fatalf("stream %d never printed %q", stream, marker)
		}
	}
}

// fakeHost serves just enough of the wire to break flow control toward a
// client: grant-flood sends past the client's window, grant-credit grants
// input credit nobody used, grant-ok prints "ok". It reports detaches.
func fakeHost(t *testing.T) (string, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	detached := make(chan string, 16)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		sess := newSession(conn)
		defer sess.Close()
		for {
			f, err := sess.Recv()
			if err != nil {
				return
			}
			if f.Kind != kindRequest {
				continue
			}
			var extra []byte
			switch f.Method {
			case methodAuth:
				extra, _ = encodeExtra(authResult{Protocol: protocolVersion})
			case methodEnsure:
				extra, _ = encodeExtra(ensureResult{Epoch: "epoch"})
			case methodReady:
				extra, _ = encodeExtra(execenv.Report{})
			case methodDetach:
				detached <- f.Grant
			case methodAttach:
				var wrapped epochArgs
				var args attachArgs
				_ = decodeExtra(f.Extra, &wrapped)
				_ = decodeExtra(wrapped.Body, &args)
				extra, _ = encodeExtra(attachResult{Terminal: execenv.TerminalID("t-" + f.Grant), Credit: ptyWindow})
				_ = sess.Send(frame{Seq: f.Seq, Kind: kindResponse, Method: f.Method, Grant: f.Grant, Extra: extra})
				switch f.Grant {
				case "grant-flood":
					chunk := bytes.Repeat([]byte("y"), mux.MaxPtyBytes)
					for i := range uint64(ptyWindow/mux.MaxPtyBytes + 1) {
						_ = sess.Send(frame{Seq: i * mux.MaxPtyBytes, Kind: kindPty, Grant: f.Grant, Stream: args.Stream, Extra: chunk})
					}
				case "grant-credit":
					_ = sess.Send(frame{Seq: 1, Kind: kindCredit, Grant: f.Grant, Stream: args.Stream})
				case "grant-ok":
					_ = sess.Send(frame{Kind: kindPty, Grant: f.Grant, Stream: args.Stream, Extra: []byte("ok")})
				}
				continue
			}
			_ = sess.Send(frame{Seq: f.Seq, Kind: kindResponse, Method: f.Method, Grant: f.Grant, Extra: extra})
		}
	}()
	return ln.Addr().String(), detached
}

func waitDetached(t *testing.T, detached chan string, grant string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-detached:
			if got == grant {
				return
			}
		case <-timeout:
			t.Fatalf("client never detached %s", grant)
		}
	}
}
