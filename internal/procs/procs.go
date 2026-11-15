// Package procs runs a grant's processes above whatever starts them.
//
// It keeps each process's output history per stream, by absolute offset, so
// any number of readers can attach and resume where they left off; it takes
// input by offset, so a resend duplicates nothing; it ends a process left
// unattached past its grace, or running past its timeout; and it freezes
// and ends with the grant. The adapter supplies only how a process starts.
package procs

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
)

// Backend is one running process, as the adapter started it.
type Backend interface {
	// Streams are the process's outputs: StreamPTY, or StreamStdout and
	// StreamStderr. Each ends with io.EOF once the process closes it.
	Streams() map[execenv.Stream]io.Reader
	// Write sends input.
	Write(p []byte) (int, error)
	CloseInput() error
	Resize(ctx context.Context, w execenv.Window) error
	Signal(s execenv.Signal) error
	// Wait blocks until the process has exited.
	Wait() execenv.ExitStatus
	PID() int
}

// Starter starts a process for spec.
type Starter func(ctx context.Context, spec execenv.ProcessSpec) (Backend, error)

// killDelay is how long a timed-out process has between TERM and KILL.
const killDelay = 5 * time.Second

// Manager is one grant's processes.
type Manager struct {
	start Starter

	mu     sync.Mutex
	procs  map[string]*proc
	frozen bool
	ended  error
}

// New returns a manager that starts processes with start.
func New(start Starter) *Manager {
	return &Manager{start: start, procs: make(map[string]*proc)}
}

type proc struct {
	m       *Manager
	spec    execenv.ProcessSpec
	backend Backend
	started time.Time
	rings   map[execenv.Stream]*ring
	cond    *sync.Cond

	input     uint64
	inputMu   sync.Mutex
	exited    bool
	exit      execenv.ExitStatus
	streaming int
	attached  int
	grace     *time.Timer
	timeout   *time.Timer
}

func (m *Manager) guard() error {
	if m.ended != nil {
		return m.ended
	}
	if m.frozen {
		return execenv.ErrFrozen
	}
	return nil
}

// Start starts spec. A tag still running is ErrConflict; one that exited
// is replaced.
func (m *Manager) Start(ctx context.Context, spec execenv.ProcessSpec) (execenv.ProcessInfo, error) {
	if err := execenv.ValidateProcessSpec(spec); err != nil {
		return execenv.ProcessInfo{}, err
	}
	if spec.HistoryBytes == 0 {
		spec.HistoryBytes = execenv.DefaultHistoryBytes
	}
	m.mu.Lock()
	if err := m.guard(); err != nil {
		m.mu.Unlock()
		return execenv.ProcessInfo{}, err
	}
	if old, ok := m.procs[spec.Tag]; ok && !old.exited {
		m.mu.Unlock()
		return execenv.ProcessInfo{}, execenv.ErrConflict
	}
	running := 0
	for _, p := range m.procs {
		if !p.exited {
			running++
		}
	}
	if running >= execenv.MaxProcesses {
		m.mu.Unlock()
		return execenv.ProcessInfo{}, execenv.ErrBusy
	}
	// Hold the tag while the backend starts.
	placeholder := &proc{m: m, spec: spec, exited: false}
	placeholder.cond = sync.NewCond(&m.mu)
	m.procs[spec.Tag] = placeholder
	m.mu.Unlock()

	backend, err := m.start(ctx, spec)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		delete(m.procs, spec.Tag)
		return execenv.ProcessInfo{}, err
	}
	p := placeholder
	p.backend = backend
	p.started = time.Now()
	p.rings = make(map[execenv.Stream]*ring)
	streams := backend.Streams()
	for name := range streams {
		p.rings[name] = newRing(int(spec.HistoryBytes))
	}
	if spec.PTY == nil && !spec.Stdin {
		_ = backend.CloseInput()
	}
	p.streaming = len(streams)
	for name, r := range streams {
		go p.pump(name, r)
	}
	go p.wait()
	if spec.Timeout > 0 {
		p.timeout = time.AfterFunc(spec.Timeout, func() {
			_ = backend.Signal(execenv.SignalTerminate)
			time.AfterFunc(killDelay, func() { _ = backend.Signal(execenv.SignalKill) })
		})
	}
	p.armGraceLocked()
	if m.ended != nil {
		_ = backend.Signal(execenv.SignalKill)
	}
	return p.infoLocked(), nil
}

func (p *proc) pump(name execenv.Stream, r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			p.m.mu.Lock()
			p.rings[name].write(buf[:n])
			p.cond.Broadcast()
			p.m.mu.Unlock()
		}
		if err != nil {
			p.m.mu.Lock()
			p.streaming--
			p.cond.Broadcast()
			p.m.mu.Unlock()
			return
		}
	}
}

func (p *proc) wait() {
	status := p.backend.Wait()
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	p.exited, p.exit = true, status
	if p.timeout != nil {
		p.timeout.Stop()
	}
	if p.grace != nil {
		p.grace.Stop()
	}
	p.cond.Broadcast()
}

// armGraceLocked starts the detach grace when nothing is attached.
func (p *proc) armGraceLocked() {
	if p.spec.DetachGrace <= 0 || p.attached > 0 || p.exited {
		return
	}
	if p.grace != nil {
		p.grace.Stop()
	}
	backend := p.backend
	p.grace = time.AfterFunc(p.spec.DetachGrace, func() {
		p.m.mu.Lock()
		idle := p.attached == 0 && !p.exited
		p.m.mu.Unlock()
		if idle {
			_ = backend.Signal(execenv.SignalHangup)
			time.AfterFunc(killDelay, func() { _ = backend.Signal(execenv.SignalKill) })
		}
	})
}

func (p *proc) infoLocked() execenv.ProcessInfo {
	out := execenv.ProcessInfo{Tag: p.spec.Tag, PTY: p.spec.PTY != nil, Started: p.started, Running: !p.exited, Exit: p.exit, Input: p.input, Output: make(map[execenv.Stream]uint64)}
	if p.backend != nil {
		out.PID = p.backend.PID()
	}
	for name, r := range p.rings {
		out.Output[name] = r.total
	}
	return out
}

func (m *Manager) get(tag string) (*proc, error) {
	p, ok := m.procs[tag]
	if !ok || p.backend == nil {
		return nil, execenv.ErrNotFound
	}
	return p, nil
}

// Process describes tag.
func (m *Manager) Process(tag string) (execenv.ProcessInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended != nil {
		return execenv.ProcessInfo{}, m.ended
	}
	p, err := m.get(tag)
	if err != nil {
		return execenv.ProcessInfo{}, err
	}
	return p.infoLocked(), nil
}

// List describes every process, running or exited.
func (m *Manager) List() ([]execenv.ProcessInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended != nil {
		return nil, m.ended
	}
	out := make([]execenv.ProcessInfo, 0, len(m.procs))
	for _, p := range m.procs {
		if p.backend != nil {
			out = append(out, p.infoLocked())
		}
	}
	return out, nil
}

// WriteInput appends data at offset.
func (m *Manager) WriteInput(tag string, offset uint64, data []byte) (uint64, error) {
	m.mu.Lock()
	if err := m.guard(); err != nil {
		m.mu.Unlock()
		return 0, err
	}
	p, err := m.get(tag)
	if err != nil {
		m.mu.Unlock()
		return 0, err
	}
	if p.spec.PTY == nil && !p.spec.Stdin {
		m.mu.Unlock()
		return 0, execenv.ErrInvalid
	}
	m.mu.Unlock()
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	m.mu.Lock()
	current, exited := p.input, p.exited
	m.mu.Unlock()
	if offset > current {
		return current, execenv.ErrConflict
	}
	skip := current - offset
	if skip >= uint64(len(data)) {
		return current, nil
	}
	if exited {
		return current, execenv.ErrClosed
	}
	rest := data[skip:]
	n, err := p.backend.Write(rest)
	m.mu.Lock()
	p.input += uint64(n)
	current = p.input
	m.mu.Unlock()
	if err != nil {
		return current, execenv.ErrClosed
	}
	return current, nil
}

func (m *Manager) live(tag string) (*proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(); err != nil {
		return nil, err
	}
	p, err := m.get(tag)
	if err != nil {
		return nil, err
	}
	if p.exited {
		return nil, execenv.ErrClosed
	}
	return p, nil
}

func (m *Manager) CloseInput(tag string) error {
	p, err := m.live(tag)
	if err != nil {
		return err
	}
	return p.backend.CloseInput()
}

func (m *Manager) Resize(ctx context.Context, tag string, w execenv.Window) error {
	p, err := m.live(tag)
	if err != nil {
		return err
	}
	if p.spec.PTY == nil {
		return execenv.ErrInvalid
	}
	return p.backend.Resize(ctx, w)
}

func (m *Manager) Signal(tag string, s execenv.Signal) error {
	switch s {
	case execenv.SignalInterrupt, execenv.SignalTerminate, execenv.SignalKill, execenv.SignalHangup, execenv.SignalQuit:
	default:
		return execenv.ErrInvalid
	}
	p, err := m.live(tag)
	if err != nil {
		return err
	}
	return p.backend.Signal(s)
}

// Freeze refuses input and starts until Thaw. Output already produced
// stays readable.
func (m *Manager) Freeze() {
	m.mu.Lock()
	m.frozen = true
	m.mu.Unlock()
}

func (m *Manager) Thaw() {
	m.mu.Lock()
	m.frozen = false
	m.mu.Unlock()
}

// End kills every process and fails every call and attachment with cause.
func (m *Manager) End(cause error) {
	m.mu.Lock()
	if m.ended != nil {
		m.mu.Unlock()
		return
	}
	m.ended = cause
	var backends []Backend
	for _, p := range m.procs {
		if p.backend != nil && !p.exited {
			backends = append(backends, p.backend)
		}
		p.cond.Broadcast()
	}
	m.mu.Unlock()
	for _, b := range backends {
		_ = b.Signal(execenv.SignalKill)
	}
}

// Live reports whether any process is running.
func (m *Manager) Live() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		if p.backend != nil && !p.exited {
			return true
		}
	}
	return false
}

// Attach reads tag's output from the offsets in from.
func (m *Manager) Attach(tag string, from map[execenv.Stream]uint64) (execenv.Attachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended != nil {
		return nil, m.ended
	}
	p, err := m.get(tag)
	if err != nil {
		return nil, err
	}
	a := &attachment{p: p, offsets: make(map[execenv.Stream]uint64), names: make([]execenv.Stream, 0, len(p.rings))}
	for name, r := range p.rings {
		at, ok := from[name]
		if !ok {
			at = r.first()
		}
		if at > r.total {
			return nil, execenv.ErrInvalid
		}
		a.offsets[name] = at
		a.names = append(a.names, name)
	}
	for name := range from {
		if _, ok := p.rings[name]; !ok {
			return nil, execenv.ErrInvalid
		}
	}
	p.attached++
	if p.grace != nil {
		p.grace.Stop()
	}
	return a, nil
}

type attachment struct {
	p       *proc
	offsets map[execenv.Stream]uint64
	names   []execenv.Stream
	next    int
	closed  bool
	done    bool
}

func (a *attachment) Next(ctx context.Context) (execenv.Output, error) {
	m := a.p.m
	stop := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		a.p.cond.Broadcast()
		m.mu.Unlock()
	})
	defer stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		if a.closed {
			return execenv.Output{}, execenv.ErrClosed
		}
		if m.ended != nil {
			return execenv.Output{}, m.ended
		}
		if err := ctx.Err(); err != nil {
			return execenv.Output{}, err
		}
		if a.done {
			return execenv.Output{}, io.EOF
		}
		for range a.names {
			name := a.names[a.next%len(a.names)]
			a.next++
			r := a.p.rings[name]
			at := a.offsets[name]
			if first := r.first(); at < first {
				a.offsets[name] = first
				return execenv.Output{Kind: execenv.OutputTruncated, Stream: name, Offset: at, To: first}, nil
			}
			if at < r.total {
				data := r.read(at, 32<<10)
				a.offsets[name] = at + uint64(len(data))
				return execenv.Output{Kind: execenv.OutputData, Stream: name, Offset: at, Data: data}, nil
			}
		}
		if a.p.exited && a.p.streaming == 0 {
			a.done = true
			return execenv.Output{Kind: execenv.OutputExit, Exit: a.p.exit}, nil
		}
		a.p.cond.Wait()
	}
}

func (a *attachment) Close() error {
	m := a.p.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	a.p.attached--
	a.p.armGraceLocked()
	a.p.cond.Broadcast()
	return nil
}

// ring keeps the newest bytes of one stream by absolute offset.
type ring struct {
	buf   []byte
	total uint64
}

func newRing(size int) *ring { return &ring{buf: make([]byte, size)} }

func (r *ring) first() uint64 {
	if r.total > uint64(len(r.buf)) {
		return r.total - uint64(len(r.buf))
	}
	return 0
}

func (r *ring) write(p []byte) {
	size := uint64(len(r.buf))
	if uint64(len(p)) > size {
		r.total += uint64(len(p)) - size
		p = p[uint64(len(p))-size:]
	}
	for len(p) > 0 {
		at := r.total % size
		n := copy(r.buf[at:], p)
		p = p[n:]
		r.total += uint64(n)
	}
}

// read copies up to max bytes from offset, which must be within the ring.
func (r *ring) read(offset uint64, max int) []byte {
	size := uint64(len(r.buf))
	at := offset % size
	end := min(size, at+(r.total-offset))
	n := min(int(end-at), max)
	return append([]byte(nil), r.buf[at:at+uint64(n)]...)
}
