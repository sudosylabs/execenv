// Package execenvtest contains a reusable Host conformance suite.
//
// Run covers occupancy, one PTY, tree projection, and guest harvest when
// the host implements GuestWriter. Adapters that wrap another host (the
// remote client) skip harvest unless they also implement that hook.
package execenvtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
)

// Factory returns a Host isolated enough for one conformance subtest.
// The host must advertise images "default" and "alt" and accept two grants.
type Factory func(t *testing.T) execenv.Host

// Run exercises the portable occupancy and PTY contract.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("ready reports the default image", func(t *testing.T) {
		host := factory(t)
		report, err := host.Ready(context.Background())
		if err != nil {
			t.Fatalf("Ready() error = %v", err)
		}
		if !report.Usable {
			t.Fatal("Ready() Usable = false")
		}
		if !hasImage(report.Images, "default") {
			t.Fatalf("Ready() Images = %v, want default", report.Images)
		}
		if report.Slots < 1 {
			t.Fatalf("Ready() Slots = %d, want at least 1", report.Slots)
		}
	})

	t.Run("ensure occupies a grant", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{
			ID:    "grant-1",
			Image: "default",
		})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		if env.ID() != "grant-1" {
			t.Fatalf("ID() = %q, want grant-1", env.ID())
		}
	})

	t.Run("ensure reattaches the same grant", func(t *testing.T) {
		host := factory(t)
		first, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		second, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("second Ensure() error = %v", err)
		}
		if first.ID() != second.ID() {
			t.Fatalf("IDs = %q and %q", first.ID(), second.ID())
		}
	})

	t.Run("ensure rejects an unknown image", func(t *testing.T) {
		host := factory(t)
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "missing"})
		if !errors.Is(err, execenv.ErrUnknownImage) {
			t.Fatalf("Ensure() error = %v, want ErrUnknownImage", err)
		}
	})

	t.Run("ensure rejects an invalid spec", func(t *testing.T) {
		host := factory(t)
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "bad id", Image: "default"})
		if !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("Ensure() error = %v, want ErrInvalid", err)
		}
	})

	t.Run("ensure conflicts when image or network change", func(t *testing.T) {
		host := factory(t)
		if _, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "alt"})
		if !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Ensure() error = %v, want ErrConflict", err)
		}
		_, err = host.Ensure(context.Background(), execenv.Spec{
			ID:      "grant-1",
			Image:   "default",
			Network: execenv.NetworkAllowlist,
		})
		if !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Ensure() network error = %v, want ErrConflict", err)
		}
	})

	t.Run("ensure refuses a third grant when only two slots exist", func(t *testing.T) {
		host := factory(t)
		if _, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		if _, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-2", Image: "default"}); err != nil {
			t.Fatalf("second Ensure() error = %v", err)
		}
		_, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-3", Image: "default"})
		if !errors.Is(err, execenv.ErrCapacity) {
			t.Fatalf("Ensure() error = %v, want ErrCapacity", err)
		}
	})

	t.Run("attach accepts pty writes", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		// Isolated attach is a guest shell, not an echo. Portable
		// coverage is that a write is accepted; echo is adapter-local.
		if _, err := term.Write([]byte("ping")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	})

	t.Run("second attach is busy until close", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		first, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		_, err = env.Attach(context.Background(), execenv.Window{})
		if !errors.Is(err, execenv.ErrBusy) {
			t.Fatalf("second Attach() error = %v, want ErrBusy", err)
		}
		if err := first.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		second, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() after Close() error = %v", err)
		}
		_ = second.Close()
	})

	t.Run("freeze blocks pty and thaw restores it", func(t *testing.T) {
		host := factory(t)
		if !host.Capabilities().Freeze {
			t.Skip("host does not advertise freeze")
		}
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		if err := env.Freeze(context.Background()); err != nil {
			t.Fatalf("Freeze() error = %v", err)
		}
		if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Write() error = %v, want ErrFrozen", err)
		}
		if _, err := env.Attach(context.Background(), execenv.Window{}); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Attach() error = %v, want ErrFrozen", err)
		}
		if err := env.Thaw(context.Background()); err != nil {
			t.Fatalf("Thaw() error = %v", err)
		}
		if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Write() after Thaw() on old terminal error = %v, want ErrFrozen", err)
		}
		term, err = env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() after Thaw() error = %v", err)
		}
		t.Cleanup(func() { _ = term.Close() })
		if _, err := term.Write([]byte("ok")); err != nil {
			t.Fatalf("Write() after Thaw() error = %v", err)
		}
	})

	t.Run("revoke is idempotent and forbids later use", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		term, err := env.Attach(context.Background(), execenv.Window{})
		if err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
		if err := env.Revoke(context.Background()); err != nil {
			t.Fatalf("Revoke() error = %v", err)
		}
		if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("Write() after Revoke() error = %v, want ErrRevoked", err)
		}
		if err := host.Revoke(context.Background(), "grant-1"); err != nil {
			t.Fatalf("second Revoke() error = %v", err)
		}
		if _, err := env.Attach(context.Background(), execenv.Window{}); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("Attach() error = %v, want ErrRevoked", err)
		}
		_, err = host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() after revoke error = %v", err)
		}
	})

	t.Run("replace tree then open the file", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		err = env.ReplaceTree(context.Background(), execenv.Tree{{
			Path:    "src/main.go",
			Kind:    execenv.KindFile,
			Version: "v1",
			Data:    []byte("package main\n"),
		}})
		if err != nil {
			t.Fatalf("ReplaceTree() error = %v", err)
		}
		body, err := env.Open(context.Background(), "src/main.go")
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		defer body.Close()
		got, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if !bytes.Equal(got, []byte("package main\n")) {
			t.Fatal("Open() returned unexpected bytes")
		}
	})

	t.Run("replace tree skips an unchanged version", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		first := execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v1", Data: []byte("one")}}
		if err := env.ReplaceTree(context.Background(), first); err != nil {
			t.Fatalf("ReplaceTree() error = %v", err)
		}
		// Nil Data means "keep the body you already have at v1".
		skip := execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v1"}}
		if err := env.ReplaceTree(context.Background(), skip); err != nil {
			t.Fatalf("version-skip ReplaceTree() error = %v", err)
		}
		missing := execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v2"}}
		if err := env.ReplaceTree(context.Background(), missing); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("missing-body ReplaceTree() error = %v, want ErrInvalid", err)
		}
	})

	t.Run("apply is fenced and rejected paths fail closed", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		err = env.Apply(context.Background(), execenv.Batch{Mutations: []execenv.Mutation{{
			Op:      execenv.OpCreate,
			Path:    "a.txt",
			Kind:    execenv.KindFile,
			Version: "v1",
			Data:    []byte("one"),
		}}})
		if err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
		err = env.Apply(context.Background(), execenv.Batch{Mutations: []execenv.Mutation{{
			Op:       execenv.OpReplace,
			Path:     "a.txt",
			Kind:     execenv.KindFile,
			Version:  "v2",
			Expected: "nope",
			Data:     []byte("two"),
		}}})
		if !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("fenced Apply() error = %v, want ErrConflict", err)
		}
		body, err := env.Open(context.Background(), "a.txt")
		if err != nil {
			t.Fatalf("Open() after failed Apply() error = %v", err)
		}
		defer body.Close()
		got, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte("one")) {
			t.Fatal("failed Apply() changed the file")
		}
		if err := env.Apply(context.Background(), execenv.Batch{Mutations: []execenv.Mutation{{
			Op:   execenv.OpCreate,
			Path: "/abs",
			Kind: execenv.KindFile,
			Data: []byte("x"),
		}}}); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("invalid path Apply() error = %v, want ErrInvalid", err)
		}
		if err := env.Apply(context.Background(), execenv.Batch{Mutations: []execenv.Mutation{{
			Op:   execenv.OpMove,
			From: "a.txt",
			Path: "b.txt",
		}}}); err != nil {
			t.Fatalf("Apply(move) error = %v", err)
		}
		if err := env.Apply(context.Background(), execenv.Batch{Mutations: []execenv.Mutation{{
			Op:   execenv.OpDelete,
			Path: "b.txt",
		}}}); err != nil {
			t.Fatalf("Apply(delete) error = %v", err)
		}
		if _, err := env.Open(context.Background(), "b.txt"); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("Open() after delete error = %v, want ErrNotFound", err)
		}
	})

	t.Run("replace tree rejects a directory that carries a body", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		err = env.ReplaceTree(context.Background(), execenv.Tree{{
			Path: "src",
			Kind: execenv.KindDirectory,
			Data: []byte("no"),
		}})
		if !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("ReplaceTree() error = %v, want ErrInvalid", err)
		}
	})

	t.Run("watch harvests guest writes", func(t *testing.T) {
		host := factory(t)
		writer, ok := host.(execenv.GuestWriter)
		if !ok {
			t.Skip("host cannot simulate guest writes")
		}
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		obs, err := env.Watch(context.Background())
		if err != nil {
			t.Fatalf("Watch() error = %v", err)
		}
		t.Cleanup(func() { _ = obs.Close() })
		if err := writer.WriteGuest(context.Background(), "grant-1", "out.txt", []byte("hi")); err != nil {
			t.Fatalf("WriteGuest() error = %v", err)
		}
		ev, err := obs.Next(context.Background())
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		if ev.Op != execenv.OpCreate || ev.Path != "out.txt" {
			t.Fatalf("event = %+v", ev)
		}
		body, err := env.Open(context.Background(), "out.txt")
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		got, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte("hi")) {
			t.Fatal("Open() after guest write returned unexpected bytes")
		}
		if err := writer.MoveGuest(context.Background(), "grant-1", "out.txt", "renamed.txt"); err != nil {
			t.Fatalf("MoveGuest() error = %v", err)
		}
		ev, err = obs.Next(context.Background())
		if err != nil || ev.Op != execenv.OpMove || ev.From != "out.txt" {
			t.Fatalf("move event = %+v err = %v", ev, err)
		}
		if err := writer.RemoveGuest(context.Background(), "grant-1", "renamed.txt"); err != nil {
			t.Fatalf("RemoveGuest() error = %v", err)
		}
		ev, err = obs.Next(context.Background())
		if err != nil || ev.Op != execenv.OpDelete || ev.Path != "renamed.txt" {
			t.Fatalf("delete event = %+v err = %v", ev, err)
		}
	})

	t.Run("cancelled replace returns the context error", func(t *testing.T) {
		host := factory(t)
		env, err := host.Ensure(context.Background(), execenv.Spec{ID: "grant-1", Image: "default"})
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err = env.ReplaceTree(ctx, execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v1", Data: []byte("x")}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReplaceTree() error = %v, want context.Canceled", err)
		}
	})

	t.Run("cancelled ensure returns the context error", func(t *testing.T) {
		host := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := host.Ensure(ctx, execenv.Spec{ID: "grant-1", Image: "default"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Ensure() error = %v, want context.Canceled", err)
		}
	})
}

func hasImage(images []execenv.Image, want execenv.Image) bool {
	for _, image := range images {
		if image == want {
			return true
		}
	}
	return false
}
