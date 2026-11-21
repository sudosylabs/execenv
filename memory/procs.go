package memory

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/procs"
	"github.com/sudosylabs/execenv/internal/screen"
)

// The in-memory host runs every process in its small fake shell: a
// terminal is the shell itself; a command is the shell running argv as one
// line and exiting with its code. It is a stand-in for tests, not a process.

var signals = map[execenv.Signal]int{
	execenv.SignalHangup:    1,
	execenv.SignalInterrupt: 2,
	execenv.SignalQuit:      3,
	execenv.SignalKill:      9,
	execenv.SignalTerminate: 15,
}

type shellProc struct {
	sh     *shell
	stderr io.Reader
	done   chan struct{}
	// screen is a terminal process's headless terminal.
	screen *screen.Terminal
}

func (e *environment) startProcess(_ context.Context, spec execenv.ProcessSpec) (procs.Backend, error) {
	win := execenv.Window{Cols: 80, Rows: 24}
	if spec.PTY != nil {
		win = *spec.PTY
	}
	sh := newShell(win, e.processes, spec.PTY != nil, spec.Tag, spec.Argv, func(path string, data []byte, appending bool) error {
		return e.shellWrite(spec.Tag, path, data, appending)
	})
	p := &shellProc{sh: sh, stderr: strings.NewReader(""), done: make(chan struct{})}
	if spec.PTY != nil {
		p.screen = screen.New(win)
	}
	go func() {
		sh.mu.Lock()
		for !sh.exited && !sh.closed {
			sh.cond.Wait()
		}
		sh.mu.Unlock()
		close(p.done)
		if p.screen != nil {
			// It writes no more; the screen stays readable.
			p.screen.Close()
		}
	}()
	if len(spec.Argv) > 0 {
		line := strings.Join(spec.Argv, " ")
		if len(spec.Argv) == 3 && (spec.Argv[0] == "sh" || spec.Argv[0] == "/bin/sh") && spec.Argv[1] == "-c" {
			line = spec.Argv[2]
		}
		if !spec.Stdin && spec.PTY == nil {
			line += "\nexit $?\n"
		} else {
			line += "\n"
		}
		_, _ = sh.Write([]byte(line))
	}
	return p, nil
}

func (p *shellProc) Streams() map[execenv.Stream]io.Reader {
	if p.sh.terminal {
		// The screen takes every byte the host's history does, in order.
		return map[execenv.Stream]io.Reader{execenv.StreamPTY: io.TeeReader(p.sh, p.screen)}
	}
	return map[execenv.Stream]io.Reader{execenv.StreamStdout: p.sh, execenv.StreamStderr: p.stderr}
}

func (p *shellProc) Write(b []byte) (int, error) { return p.sh.Write(b) }

func (p *shellProc) CloseInput() error {
	if p.sh.terminal {
		return nil
	}
	// A command reads its input to the end, then exits.
	_, err := p.sh.Write([]byte("\nexit $?\n"))
	return err
}

func (p *shellProc) Resize(ctx context.Context, w execenv.Window) error {
	if err := p.sh.Resize(ctx, w); err != nil {
		return err
	}
	p.screen.Resize(w)
	return nil
}

func (p *shellProc) Screen(context.Context) (execenv.Screen, error) {
	if p.screen == nil {
		return execenv.Screen{}, execenv.ErrInvalid
	}
	return p.screen.Snapshot(), nil
}

func (p *shellProc) Signal(s execenv.Signal) error {
	n, ok := signals[s]
	if !ok {
		return execenv.ErrInvalid
	}
	p.sh.mu.Lock()
	if !p.sh.exited && !p.sh.closed {
		p.sh.signal = n
	}
	p.sh.mu.Unlock()
	return p.sh.Close()
}

func (p *shellProc) Wait() (execenv.ExitStatus, time.Time) {
	<-p.done
	p.sh.mu.Lock()
	defer p.sh.mu.Unlock()
	if p.sh.signal != 0 {
		return execenv.ExitStatus{Signal: p.sh.signal}, p.sh.ended
	}
	return execenv.ExitStatus{Code: p.sh.code}, p.sh.ended
}

func (p *shellProc) Started() time.Time { return p.sh.started }

func (p *shellProc) PID() int { return p.sh.pid }
