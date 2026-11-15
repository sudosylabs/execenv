package execenvtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// FilesGuest acts inside env's workspace through its file API, whose
// writes are guest changes made by no started process. It suits a host
// whose workspace a test cannot reach directly.
func FilesGuest(env execenv.Env, fence execenv.ProjectionFence) Guest {
	return filesGuest{env: env, fence: fence}
}

type filesGuest struct {
	env   execenv.Env
	fence execenv.ProjectionFence
}

func (g filesGuest) Write(ctx context.Context, path string, data []byte) error {
	_, err := g.env.WriteFile(ctx, g.fence, path, bytes.NewReader(data), execenv.WriteOptions{})
	return err
}
func (g filesGuest) Mkdir(ctx context.Context, path string) error {
	return g.env.MakeDirectory(ctx, g.fence, path, false)
}
func (g filesGuest) Move(ctx context.Context, from, to string) error {
	return g.env.Move(ctx, g.fence, from, to)
}
func (g filesGuest) Remove(ctx context.Context, path string) error {
	return g.env.Remove(ctx, g.fence, path, true)
}

// run starts line as sh -c under tag and waits for it to exit.
func (w *watcher) run(tag, line string) {
	w.t.Helper()
	if _, err := w.o.Processes.Start(w.t.Context(), w.o.Fence, execenv.ProcessSpec{Tag: tag, Argv: []string{"sh", "-c", line}}); err != nil {
		w.t.Fatalf("Start(%s): %v", tag, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := w.o.Processes.Process(w.t.Context(), tag)
		if err != nil {
			w.t.Fatalf("Process(%s): %v", tag, err)
		}
		if !info.Running {
			if info.Exit != (execenv.ExitStatus{}) {
				w.t.Fatalf("%s exited %+v", tag, info.Exit)
			}
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("%s did not exit", tag)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tags drains every new observation into "operation path" -> tag.
func (w *watcher) tags() map[string]string {
	w.t.Helper()
	out := map[string]string{}
	for {
		page, err := w.o.Workspace.Observe(w.t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, After: w.host, Limit: execenv.MaxProjectionMutations})
		if err != nil {
			w.t.Fatal(err)
		}
		for _, event := range page.Observations {
			key := fmt.Sprintf("%s %s", event.Operation, event.Path)
			if event.DestinationPath != "" {
				key += " -> " + event.DestinationPath
			}
			out[key] = event.ProcessTag
			w.host = event.HostSequence
		}
		if len(page.Observations) == 0 {
			return out
		}
	}
}

// RunObservations exercises the observation stream against guest-originated
// changes: waiting for one, and, given Processes, the tag of the started
// process each change's writer descends from. It needs a real or simulated
// guest, so it is separate from Run.
//
// The tag subtests start commands of the form sh -c 'echo WORD > PATH',
// which every adapter's shell runs.
func RunObservations(t *testing.T, factory ObservedFactory) {
	t.Helper()

	t.Run("an observation names the started process that wrote it", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		if w.o.Processes == nil {
			t.Skip("the host does not run processes in this harness")
		}
		w.run("writer", "echo one > tagged.txt")
		w.write("untagged.txt", "x")
		tags := w.tags()
		if tags["create_file tagged.txt"] != "writer" || tags["create_file untagged.txt"] != "" || len(tags) != 2 {
			t.Fatalf("tags = %q, want tagged.txt by writer and untagged.txt by none", tags)
		}
	})

	t.Run("a path several processes wrote names the last", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		if w.o.Processes == nil {
			t.Skip("the host does not run processes in this harness")
		}
		w.run("first", "echo one > shared.txt")
		w.run("second", "echo two > shared.txt")
		if tags := w.tags(); tags["create_file shared.txt"] != "second" || len(tags) != 1 {
			t.Fatalf("tags = %q, want shared.txt by second", tags)
		}
		w.run("third", "echo three >> shared.txt")
		if tags := w.tags(); tags["write_file shared.txt"] != "third" || len(tags) != 1 {
			t.Fatalf("tags = %q, want shared.txt by third", tags)
		}
	})

	t.Run("a wait with nothing new returns an empty page once it passes", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		began := time.Now()
		page, err := w.o.Workspace.Observe(t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, Limit: 16, Wait: 150 * time.Millisecond})
		if err != nil || len(page.Observations) != 0 {
			t.Fatalf("Observe(wait) = %+v, %v; want an empty page", page, err)
		}
		if waited := time.Since(began); waited < 150*time.Millisecond {
			t.Fatalf("Observe(wait) returned after %v, before its wait", waited)
		}
	})

	t.Run("a wait ends as soon as the guest changes something", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		done := make(chan error, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			done <- w.o.Guest.Write(t.Context(), "late.txt", []byte("x"))
		}()
		began := time.Now()
		page, err := w.o.Workspace.Observe(t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, Limit: 16, Wait: execenv.MaxObserveWait})
		if err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(page.Observations) != 1 || page.Observations[0].Path != "late.txt" {
			t.Fatalf("Observe(wait) = %+v, want the late write", page.Observations)
		}
		if waited := time.Since(began); waited > 10*time.Second {
			t.Fatalf("Observe(wait) took %v after the write", waited)
		}
	})

	t.Run("a wait returns at once when there is already something after its position", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		w.write("early.txt", "x")
		began := time.Now()
		page, err := w.o.Workspace.Observe(t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, Limit: 16, Wait: execenv.MaxObserveWait})
		if err != nil || len(page.Observations) != 1 {
			t.Fatalf("Observe(wait) = %+v, %v", page, err)
		}
		if waited := time.Since(began); waited > 10*time.Second {
			t.Fatalf("Observe(wait) waited %v with an observation pending", waited)
		}
	})

	t.Run("a wait outside its bounds is refused", func(t *testing.T) {
		w := start(t, factory, nil, nil)
		for _, wait := range []time.Duration{-time.Millisecond, execenv.MaxObserveWait + time.Millisecond} {
			if _, err := w.o.Workspace.Observe(t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, Limit: 16, Wait: wait}); !errors.Is(err, execenv.ErrInvalid) {
				t.Fatalf("Observe(wait %v) error = %v, want ErrInvalid", wait, err)
			}
		}
	})
}
