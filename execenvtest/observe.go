package execenvtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// drain reads every new observation into seen, by "operation path".
func (w *watcher) drain() {
	w.t.Helper()
	for {
		page := w.page(w.host)
		if len(page.Observations) == 0 {
			return
		}
		for _, event := range page.Observations {
			if w.seen == nil {
				w.seen = map[string]execenv.ExecutionObservation{}
			}
			w.seen[fmt.Sprintf("%s %s", event.Operation, event.Path)] = event
			w.host = event.HostSequence
		}
	}
}

func (w *watcher) page(after uint64) execenv.ObservationPage {
	w.t.Helper()
	page, err := w.o.Workspace.Observe(w.t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, After: after, Limit: execenv.MaxProjectionMutations})
	if err != nil {
		w.t.Fatal(err)
	}
	return page
}

// mutation is the next journal mutation of path: body names an uploaded
// file body, "" none.
func (w *watcher) mutation(operation execenv.ProjectionOperation, path, body string) execenv.ProjectionMutation {
	w.t.Helper()
	m := execenv.ProjectionMutation{JournalCursor: w.workspace + 1, EntryID: w.entries[path], Operation: operation, Kind: execenv.ProjectionFile, Path: path}
	if m.EntryID == "" {
		m.EntryID = "entry-" + strings.ReplaceAll(path, "/", "-")
	}
	if operation == execenv.ProjectionDelete && w.versions[path] == "" {
		m.Kind = execenv.ProjectionDirectory
	}
	if operation != execenv.ProjectionCreate && m.Kind == execenv.ProjectionFile {
		m.ExpectedContentVersion = execenv.Version(w.versions[path])
	}
	if body != "" {
		ref, err := w.o.Workspace.UploadProjectionContent(w.t.Context(), w.o.Fence, strings.NewReader(body))
		if err != nil {
			w.t.Fatal(err)
		}
		m.Content = &ref
		m.ResultingContentVersion = execenv.Version(fmt.Sprintf("j%d", m.JournalCursor))
	}
	return m
}

// apply applies one mutation at the watcher's cursors.
func (w *watcher) apply(m execenv.ProjectionMutation) (execenv.ProjectionReceipt, error) {
	w.generation++
	return w.applyAs(m, fmt.Sprintf("apply-m%d", w.generation))
}

func (w *watcher) applyAs(m execenv.ProjectionMutation, id string) (execenv.ProjectionReceipt, error) {
	m.JournalCursor = w.workspace + 1
	if w.applied[id] {
		// A retry is the request already applied, at its own cursors.
		m.JournalCursor = w.workspace
	}
	from := m.JournalCursor - 1
	return w.o.Workspace.ApplyProjection(w.t.Context(), execenv.ProjectionApply{Fence: w.o.Fence, MutationID: id, FromWorkspaceCursor: from, ThroughWorkspaceCursor: from + 1, ExpectedHostCursor: w.host, Mutations: []execenv.ProjectionMutation{m}})
}

// committed records an applied mutation in the watcher's journal.
func (w *watcher) committed(m execenv.ProjectionMutation) {
	w.workspace++
	w.applied[fmt.Sprintf("apply-m%d", w.generation)] = true
	switch m.Operation {
	case execenv.ProjectionDelete:
		delete(w.entries, m.Path)
		delete(w.versions, m.Path)
	default:
		w.entries[m.Path] = m.EntryID
		w.versions[m.Path] = string(m.ResultingContentVersion)
	}
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
// changes: waiting for one, a projection that overwrites unconfirmed ones,
// and, given Processes, the tag of the started process each change's
// writer descends from. It needs a real or simulated
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

	t.Run("an overwrite replaces an unconfirmed guest write and discards it", func(t *testing.T) {
		w := start(t, factory, []seed{{path: "a.txt", body: "base"}}, nil)
		w.write("a.txt", "guest")
		w.write("other.txt", "kept")
		w.drain()
		// A guest may write in place or replace the file: every observation
		// of a.txt is the guest's unconfirmed change.
		var onA []uint64
		var guest execenv.ExecutionObservation
		for _, event := range w.page(0).Observations {
			if event.Path == "a.txt" {
				onA = append(onA, event.HostSequence)
				if event.Content != nil {
					guest = event
				}
			}
		}
		if len(onA) == 0 || guest.Content == nil {
			t.Fatalf("the guest write of a.txt was not observed: %v", w.seen)
		}
		write := w.mutation(execenv.ProjectionWrite, "a.txt", "journal")
		if _, err := w.apply(write); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Apply over an unconfirmed guest write = %v, want ErrConflict", err)
		}
		write.Overwrite = true
		receipt, err := w.apply(write)
		if err != nil {
			t.Fatalf("Apply(overwrite) = %v", err)
		}
		if !slices.Equal(receipt.Discarded, onA) {
			t.Fatalf("Discarded = %v, want %v", receipt.Discarded, onA)
		}
		w.committed(write)
		page := w.page(0)
		for _, event := range page.Observations {
			want := uint64(0)
			if slices.Contains(onA, event.HostSequence) {
				want = write.JournalCursor
			}
			if event.DiscardedBy != want {
				t.Fatalf("%s %s DiscardedBy = %d, want %d", event.Operation, event.Path, event.DiscardedBy, want)
			}
		}
		binding := execenv.ObservationProjection{Fence: w.o.Fence, HostSequence: guest.HostSequence, Mutation: w.mutation(execenv.ProjectionWrite, "a.txt", "again")}
		binding.Mutation.Content.Size, binding.Mutation.Content.Digest = guest.Content.Size, guest.Content.Digest
		if err := w.o.Workspace.ConfirmObservationProjection(t.Context(), binding); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Confirm of a discarded observation = %v, want ErrConflict", err)
		}
		// The journal's body is in place: the overwrite itself is no
		// observation, and an exact retry returns the same receipt.
		w.expect()
		again, err := w.applyAs(write, receipt.MutationID)
		if err != nil || !slices.Equal(again.Discarded, receipt.Discarded) {
			t.Fatalf("retry of the overwrite = %+v, %v", again, err)
		}
		if err := w.o.Workspace.AcknowledgeObservations(t.Context(), w.o.Fence, w.host); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an overwriting create replaces a guest file and a delete its whole subtree", func(t *testing.T) {
		w := start(t, factory, []seed{{path: "d", dir: true}}, nil)
		w.write("new.txt", "guest")
		w.mkdir("d/x")
		w.write("d/x/f", "guest")
		w.drain()
		create := w.mutation(execenv.ProjectionCreate, "new.txt", "journal")
		create.Overwrite = true
		receipt, err := w.apply(create)
		if err != nil || len(receipt.Discarded) != 1 || receipt.Discarded[0] != w.seen["create_file new.txt"].HostSequence {
			t.Fatalf("Apply(overwriting create) = %+v, %v", receipt, err)
		}
		w.committed(create)
		recursive := true
		remove := w.mutation(execenv.ProjectionDelete, "d", "")
		remove.Recursive, remove.Overwrite = &recursive, true
		receipt, err = w.apply(remove)
		if err != nil {
			t.Fatal(err)
		}
		want := []uint64{w.seen["create_directory d/x"].HostSequence, w.seen["create_file d/x/f"].HostSequence}
		slices.Sort(want)
		if !slices.Equal(receipt.Discarded, want) {
			t.Fatalf("Discarded = %v, want %v", receipt.Discarded, want)
		}
		w.committed(remove)
		w.expect()
	})

	t.Run("an overwrite cannot discard a move across its path", func(t *testing.T) {
		w := start(t, factory, []seed{{path: "a.txt", body: "base"}}, nil)
		w.move("a.txt", "b.txt")
		w.drain()
		write := w.mutation(execenv.ProjectionWrite, "a.txt", "journal")
		write.Overwrite = true
		if _, err := w.apply(write); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("overwrite of a path the guest moved away = %v, want ErrConflict", err)
		}
		move := execenv.ProjectionMutation{EntryID: w.entries["a.txt"], Operation: execenv.ProjectionMove, Kind: execenv.ProjectionFile, Path: "a.txt", DestinationPath: "c.txt", ExpectedContentVersion: execenv.Version(w.versions["a.txt"]), Overwrite: true}
		if _, err := w.apply(move); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("an overwriting move = %v, want ErrInvalid", err)
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
