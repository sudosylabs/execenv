package agent_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
)

func TestReplaceTreeThenOpen(t *testing.T) {
	t.Parallel()
	cli, home := startAgent(t)
	err := cli.ReplaceTree(t.Context(), execenv.Tree{{
		Path:    "src/main.go",
		Kind:    execenv.KindFile,
		Version: "v1",
		Data:    []byte("package main\n"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, "src", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "package main\n" {
		t.Fatal("home did not receive the projected file")
	}
	body, err := cli.Open(t.Context(), "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(raw) != "package main\n" {
		t.Fatalf("Open() = %q err = %v", raw, err)
	}
}

func TestReplaceTreeKeepsVersion(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	first := execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v1", Data: []byte("one")}}
	if err := cli.ReplaceTree(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	skip := execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v1"}}
	if err := cli.ReplaceTree(t.Context(), skip); err != nil {
		t.Fatal(err)
	}
	missing := execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v2"}}
	if err := cli.ReplaceTree(t.Context(), missing); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("missing body error = %v, want ErrInvalid", err)
	}
}

func TestApplyLeavesGuestFile(t *testing.T) {
	t.Parallel()
	cli, home := startAgent(t)
	if err := cli.Apply(t.Context(), execenv.Batch{Mutations: []execenv.Mutation{{
		Op:      execenv.OpCreate,
		Path:    "a.txt",
		Kind:    execenv.KindFile,
		Version: "v1",
		Data:    []byte("one"),
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "guest.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cli.Apply(t.Context(), execenv.Batch{Mutations: []execenv.Mutation{{
		Op:   execenv.OpReplace,
		Path: "a.txt",
		Kind: execenv.KindFile,
		Data: []byte("two"),
	}}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, "guest.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("guest file after Apply = %q err = %v", got, err)
	}
}

func TestApplyFailureRestoresHome(t *testing.T) {
	t.Parallel()
	cli, home := startAgent(t)
	if err := cli.ReplaceTree(t.Context(), execenv.Tree{{
		Path:    "blocked",
		Kind:    execenv.KindFile,
		Version: "v1",
		Data:    []byte("old"),
	}}); err != nil {
		t.Fatal(err)
	}
	err := cli.Apply(t.Context(), execenv.Batch{Mutations: []execenv.Mutation{
		{
			Op:   execenv.OpReplace,
			Path: "blocked",
			Kind: execenv.KindFile,
			Data: []byte("new"),
		},
		{
			Op:   execenv.OpCreate,
			Path: "blocked/nested.txt",
			Kind: execenv.KindFile,
			Data: []byte("nope"),
		},
	}})
	if err == nil {
		t.Fatal("Apply() succeeded with a file parent")
	}
	got, err := os.ReadFile(filepath.Join(home, "blocked"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("home after failed Apply = %q, want old", got)
	}
}

func TestApplyReplaceDoesNotLeaveStash(t *testing.T) {
	t.Parallel()
	cli, home := startAgent(t)
	if err := cli.ReplaceTree(t.Context(), execenv.Tree{{
		Path: "a.txt", Kind: execenv.KindFile, Version: "v1", Data: []byte("one"),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := cli.Apply(t.Context(), execenv.Batch{Mutations: []execenv.Mutation{{
		Op: execenv.OpReplace, Path: "a.txt", Kind: execenv.KindFile, Data: []byte("two"),
	}}}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "a.txt" {
		t.Fatalf("home entries = %v, want only a.txt", names(entries))
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, ent := range entries {
		out = append(out, ent.Name())
	}
	return out
}

func TestTouchInPtyBecomesWatch(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	obs, err := cli.Watch(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte("touch seen.txt\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if !waitEvent(t, ctx, obs, execenv.OpCreate, "seen.txt") {
		t.Fatal("Watch did not see touch seen.txt")
	}
	body, err := cli.Open(t.Context(), "seen.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
}

func TestWatchReplaysChangesMadeWhileDetached(t *testing.T) {
	cli, home := startAgent(t)
	first, err := cli.Watch(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.Cursor()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "offline.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Let the guest-side watcher record the change before resuming, which also
	// exercises delivery of replay frames sent before the Watch response.
	time.Sleep(2 * pollIntervalForTest)
	resumed, err := cli.Watch(t.Context(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	event, err := resumed.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if event.Path != "offline.txt" || event.Cursor == "" {
		t.Fatalf("replayed event = %+v", event)
	}
}

const pollIntervalForTest = 50 * time.Millisecond

func TestClosePtyIsHangup(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cli.ReplaceTree(t.Context(), execenv.Tree{{
		Path:    "still.txt",
		Kind:    execenv.KindFile,
		Version: "v1",
		Data:    []byte("ok"),
	}}); err != nil {
		t.Fatalf("ReplaceTree after hangup: %v", err)
	}
	again, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatalf("Attach after hangup: %v", err)
	}
	_ = again.Close()
}

func TestFreezeStopsIO(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if err := cli.Freeze(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("Write after freeze = %v, want ErrFrozen", err)
	}
	if _, err := cli.Attach(t.Context(), execenv.Window{}); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("Attach after freeze = %v, want ErrFrozen", err)
	}
	if err := cli.Thaw(t.Context()); err != nil {
		t.Fatal(err)
	}
	term, err = cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	_ = term.Close()
}

func TestListenGuestIsGuestOnly(t *testing.T) {
	t.Parallel()
	ln, err := agent.ListenGuest()
	if err == nil {
		_ = ln.Close()
		if ln == nil {
			t.Fatal("ListenGuest() returned a nil listener")
		}
		return
	}
	if !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("ListenGuest() error = %v, want ErrUnavailable off a guest", err)
	}
}

func startAgent(t *testing.T) (*agent.Client, string) {
	t.Helper()
	home := t.TempDir()
	host, guest := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() {
		errc <- agent.Serve(ctx, guest, agent.Config{Home: home})
	}()
	t.Cleanup(func() {
		_ = host.Close()
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
		}
	})
	return agent.NewClient(host), home
}

func waitEvent(t *testing.T, ctx context.Context, obs execenv.Observation, op execenv.Op, path string) bool {
	t.Helper()
	for {
		ev, err := obs.Next(ctx)
		if err != nil {
			return false
		}
		if ev.Op == op && ev.Path == path {
			return true
		}
	}
}
