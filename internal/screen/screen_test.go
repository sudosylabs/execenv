package screen

import (
	"slices"
	"testing"

	"github.com/sudosylabs/execenv"
)

func newTerminal(t *testing.T, cols, rows uint16) *Terminal {
	t.Helper()
	term := New(execenv.Window{Cols: cols, Rows: rows})
	t.Cleanup(term.Close)
	return term
}

func write(t *testing.T, term *Terminal, s string) {
	t.Helper()
	if n, err := term.Write([]byte(s)); err != nil || n != len(s) {
		t.Fatalf("Write() = %d, %v", n, err)
	}
}

// A snapshot's Data redraws the same screen on a blank terminal.
func redraws(t *testing.T, s execenv.Screen) {
	t.Helper()
	again := newTerminal(t, s.Window.Cols, s.Window.Rows)
	write(t, again, string(s.Data))
	r := again.Snapshot()
	if !slices.Equal(r.Text, s.Text) || r.AltScreen != s.AltScreen || r.CursorRow != s.CursorRow || r.CursorCol != s.CursorCol || r.CursorHidden != s.CursorHidden {
		t.Fatalf("redrawn = %+v\nwant %+v", r, s)
	}
}

func TestSnapshotTracksTextCursorAndOffset(t *testing.T) {
	term := newTerminal(t, 20, 4)
	out := "$ ls\r\nfile.txt\r\n$ "
	write(t, term, out)
	s := term.Snapshot()
	if s.Offset != uint64(len(out)) || s.Window != (execenv.Window{Cols: 20, Rows: 4}) {
		t.Fatalf("offset/window = %d %+v", s.Offset, s.Window)
	}
	if want := []string{"$ ls", "file.txt", "$", ""}; !slices.Equal(s.Text, want) {
		t.Fatalf("Text = %q, want %q", s.Text, want)
	}
	if s.CursorRow != 2 || s.CursorCol != 2 || s.AltScreen || s.CursorHidden {
		t.Fatalf("cursor = %+v", s)
	}
	redraws(t, s)
}

// A full-screen program's alternate screen, styles and hidden cursor are
// what the snapshot shows; leaving it brings the main screen back.
func TestSnapshotOfAFullScreenProgram(t *testing.T) {
	term := newTerminal(t, 20, 4)
	write(t, term, "main text")
	write(t, term, "\x1b[?1049h\x1b[2J\x1b[H\x1b[1;31mtitle\x1b[m\x1b[3;5Hbody\x1b[?25l")
	s := term.Snapshot()
	if !s.AltScreen || !s.CursorHidden || s.Text[0] != "title" || s.Text[2] != "    body" {
		t.Fatalf("full screen = %+v", s)
	}
	redraws(t, s)
	write(t, term, "\x1b[?1049l\x1b[?25h")
	if s := term.Snapshot(); s.AltScreen || s.Text[0] != "main text" {
		t.Fatalf("after leaving = %+v", s)
	}
}

// Queries a program sends are answered by the real terminal, not here:
// they must not stall the output.
func TestQueriesDoNotStall(t *testing.T) {
	term := newTerminal(t, 20, 4)
	for range 1000 {
		write(t, term, "\x1b[6n\x1b[c")
	}
	write(t, term, "after")
	if s := term.Snapshot(); s.Text[0] != "after" {
		t.Fatalf("Text = %q", s.Text)
	}
}

func TestResizeChangesTheWindow(t *testing.T) {
	term := newTerminal(t, 20, 4)
	term.Resize(execenv.Window{Cols: 40, Rows: 10})
	if s := term.Snapshot(); s.Window != (execenv.Window{Cols: 40, Rows: 10}) || len(s.Text) != 10 {
		t.Fatalf("resized = %+v (%d rows)", s.Window, len(s.Text))
	}
}

// A closed terminal still takes output and still reads.
func TestClosedTerminalStaysReadable(t *testing.T) {
	term := New(execenv.Window{Cols: 20, Rows: 4})
	write(t, term, "before")
	term.Close()
	write(t, term, " after\x1b[6n")
	if s := term.Snapshot(); s.Text[0] != "before after" {
		t.Fatalf("Text = %q", s.Text)
	}
	term.Close()
}
