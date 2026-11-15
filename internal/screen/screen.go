// Package screen keeps a terminal process's screen: a headless terminal fed
// every byte the process writes to its terminal, so the screen can be read
// at any time, as of an exact output offset.
package screen

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/charmbracelet/x/vt"

	"github.com/sudosylabs/execenv"
)

// Terminal is one process's headless terminal. Write it from one goroutine,
// in output order; Snapshot and Resize may come from any.
type Terminal struct {
	mu     sync.Mutex
	emu    *vt.Emulator
	win    execenv.Window
	offset uint64
}

// New is a blank terminal of win's size.
func New(win execenv.Window) *Terminal {
	win = sized(win)
	emu := vt.NewEmulator(int(win.Cols), int(win.Rows))
	// The emulator answers a program's queries (cursor position, device
	// attributes) on its reader. Those answers are the real terminal's to
	// give, so they are read and dropped; unread, they would stall Write.
	go func() { _, _ = io.Copy(io.Discard, emu) }()
	return &Terminal{emu: emu, win: win}
}

func sized(win execenv.Window) execenv.Window {
	if win.Cols == 0 {
		win.Cols = 80
	}
	if win.Rows == 0 {
		win.Rows = 24
	}
	return win
}

// Write takes the process's next terminal output. It never fails.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = t.emu.Write(p)
	t.offset += uint64(len(p))
	return len(p), nil
}

// Resize follows the process's terminal to a new size.
func (t *Terminal) Resize(win execenv.Window) {
	win = sized(win)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emu.Resize(int(win.Cols), int(win.Rows))
	t.win = win
}

// Close stops the terminal. It closes the pipe its query answers are read
// from rather than calling the emulator's Close, which races that read.
func (t *Terminal) Close() {
	if c, ok := t.emu.InputPipe().(io.Closer); ok {
		_ = c.Close()
	}
}

// Snapshot is the screen now, as of the output it has taken.
func (t *Terminal) Snapshot() execenv.Screen {
	t.mu.Lock()
	defer t.mu.Unlock()
	cursor := t.emu.CursorPosition()
	s := execenv.Screen{
		Offset:       t.offset,
		Window:       t.win,
		AltScreen:    t.emu.IsAltScreen(),
		CursorRow:    cursor.Y,
		CursorCol:    cursor.X,
		CursorHidden: t.emu.CursorHidden(),
	}
	for _, line := range rows(t.emu.String(), int(t.win.Rows)) {
		s.Text = append(s.Text, strings.TrimRight(line, " "))
	}
	var b strings.Builder
	// Reset the terminal's state, enter the alternate screen if the program
	// is on it, clear, draw each row, then put the cursor back.
	b.WriteString("\x1b[m")
	if s.AltScreen {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[H\x1b[2J")
	for i, line := range rows(t.emu.Render(), int(t.win.Rows)) {
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[m", i+1, line)
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", s.CursorRow+1, s.CursorCol+1)
	if s.CursorHidden {
		b.WriteString("\x1b[?25l")
	} else {
		b.WriteString("\x1b[?25h")
	}
	s.Data = []byte(b.String())
	return s
}

// rows splits a rendered screen into at most n rows.
func rows(rendered string, n int) []string {
	lines := strings.Split(rendered, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return lines
}
