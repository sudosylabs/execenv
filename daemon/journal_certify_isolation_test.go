//go:build isolation

package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// This path requires the installed daemon, TLS remote client and a real guest
// containing the current agent, kernel workload hierarchy and isolation hardware.
func TestCertifyRemoteJournalPreservesLiveWork(t *testing.T) {
	client := startCertifyHost(t)
	if !client.Capabilities().Isolated {
		t.Fatal("certified host is not isolated")
	}
	env, err := client.Ensure(t.Context(), execenv.Spec{ID: "journal-certify", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Revoke(context.Background()) })
	journal := env
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: journal.Epoch(), FenceRevision: 1}
	if _, err := journal.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	reference, err := journal.UploadProjectionContent(t.Context(), fence, bytes.NewBufferString("initial\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init", WorkspaceCursor: 10, Entries: []execenv.ProjectionEntry{{EntryID: "dir", Kind: execenv.ProjectionDirectory, Path: "dir"}, {EntryID: "file", Kind: execenv.ProjectionFile, Path: "dir/file", ContentVersion: "v1", Content: &reference}}}); err != nil {
		t.Fatal(err)
	}
	term := execenvtest.OpenShell(t, env, fence, "sh")
	shell := newCertifiedShell(t, term)
	before := shell.state(t, "kept=survives")
	page, err := journal.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: 0, Limit: 128})
	if err != nil {
		t.Fatal(err)
	}
	cursor := page.HostCursor
	shell.run(t, "printf 'guest-first' > guest")
	reference, err = journal.UploadProjectionContent(t.Context(), fence, bytes.NewBufferString("saved\n"))
	if err != nil {
		t.Fatal(err)
	}
	save := execenv.ProjectionApply{Fence: fence, MutationID: "ordinary-save", FromWorkspaceCursor: 10, ThroughWorkspaceCursor: 11, ExpectedHostCursor: cursor, Mutations: []execenv.ProjectionMutation{{JournalCursor: 11, EntryID: "file", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "dir/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &reference}}}
	if _, err := journal.ApplyProjection(t.Context(), save); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("save absorbed unseen guest change: %v", err)
	}
	page, err = journal.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: cursor, Limit: 128})
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Operation != execenv.ObservationCreateFile || page.Observations[0].Path != "guest" {
		t.Fatalf("guest change not retained exactly once: %+v %v", page, err)
	}
	guest := page.Observations[0]
	save.ExpectedHostCursor = page.HostCursor
	if _, err := journal.ApplyProjection(t.Context(), save); err != nil {
		t.Fatal(err)
	}
	shell.run(t, `test "$(cat dir/file)" = saved`)
	reader, err := journal.OpenObservationContent(t.Context(), fence, *guest.Content)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(captured) != "guest-first" {
		t.Fatalf("observed content: %q %v", captured, err)
	}
	// Content is read live: once the guest changes the file, the older
	// observation is stale rather than passing off later bytes as its own.
	shell.run(t, "printf 'guest-later' > guest")
	if reader, err := journal.OpenObservationContent(t.Context(), fence, *guest.Content); err == nil {
		_, err = io.ReadAll(reader)
		_ = reader.Close()
		if !errors.Is(err, execenv.ErrStale) {
			t.Fatalf("changed content read = %v, want ErrStale", err)
		}
	} else if !errors.Is(err, execenv.ErrStale) {
		t.Fatalf("changed content = %v, want ErrStale", err)
	}
	reference, err = journal.UploadProjectionContent(t.Context(), fence, bytes.NewReader(captured))
	if err != nil {
		t.Fatal(err)
	}
	accepted := execenv.ProjectionMutation{JournalCursor: 12, EntryID: "guest-entry", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionCreate, Path: "guest", ResultingContentVersion: "v1", Content: &reference}
	if err := journal.ConfirmObservationProjection(t.Context(), execenv.ObservationProjection{Fence: fence, HostSequence: guest.HostSequence, Mutation: accepted}); err != nil {
		t.Fatal(err)
	}
	if err := journal.AcknowledgeObservations(t.Context(), fence, guest.HostSequence); err != nil {
		t.Fatal(err)
	}
	page, err = journal.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: guest.HostSequence, Limit: 128})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyProjection(t.Context(), execenv.ProjectionApply{Fence: fence, MutationID: "guest-outcome", FromWorkspaceCursor: 11, ThroughWorkspaceCursor: 12, ExpectedHostCursor: page.HostCursor, Mutations: []execenv.ProjectionMutation{accepted}}); err != nil {
		t.Fatal(err)
	}
	shell.run(t, `test "$(cat guest)" = guest-later`)
	cursor = page.HostCursor
	shell.run(t, `mv dir moved && test "$(cat moved/file)" = saved`)
	page, err = journal.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: cursor, Limit: 128})
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Operation != execenv.ObservationMove || page.Observations[0].Kind != execenv.ProjectionDirectory || page.Observations[0].Path != "dir" || page.Observations[0].DestinationPath != "moved" {
		t.Fatalf("directory move not semantic: %+v %v", page, err)
	}
	frozen := fence
	frozen.FenceRevision++
	receipt, err := journal.Control(t.Context(), execenv.ControlRequest{Fence: frozen, State: execenv.ControlFrozen})
	if err != nil || !receipt.Confirmed {
		t.Fatalf("actual guest containment: %+v %v", receipt, err)
	}
	if _, err := journal.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); !errors.Is(err, execenv.ErrConflict) {
		t.Fatal("old recovery reopened guest")
	}
	recovered := frozen
	recovered.FenceRevision++
	if _, err := journal.Control(t.Context(), execenv.ControlRequest{Fence: recovered, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	term.SetFence(recovered)
	after := shell.state(t, "true")
	if before != after || journal.Epoch() != fence.EnvironmentEpoch {
		t.Fatal("save/freeze replaced the healthy shell or epoch")
	}
}

// certifiedShell drives the guest shell and reads its PTY output. A command
// is complete when its marker appears; the marker is split in the typed line
// so the terminal's echo of the command never matches it.
type certifiedShell struct {
	term *execenvtest.ShellConn
	mu   sync.Mutex
	out  bytes.Buffer
	step int
}

func newCertifiedShell(t *testing.T, term *execenvtest.ShellConn) *certifiedShell {
	t.Helper()
	shell := &certifiedShell{term: term}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			shell.mu.Lock()
			shell.out.Write(buf[:n])
			shell.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return shell
}

// run executes command in the guest shell and waits until it succeeded.
func (s *certifiedShell) run(t *testing.T, command string) string {
	t.Helper()
	s.step++
	marker := fmt.Sprintf("done-%d.", s.step)
	if _, err := s.term.Write([]byte(fmt.Sprintf("%s && echo done-\"\"%d.\n", command, s.step))); err != nil {
		t.Fatalf("same PTY lost: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		out := s.out.String()
		s.mu.Unlock()
		if strings.Contains(out, marker) {
			return out
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("guest command %q did not succeed", command)
	return ""
}

var shellState = regexp.MustCompile(`state=([0-9]+:[a-z]*)`)

// state runs prefix, then prints the shell's PID and $kept. Equal results
// prove the same shell process and its unexported state survived.
func (s *certifiedShell) state(t *testing.T, prefix string) string {
	t.Helper()
	out := s.run(t, prefix+`; printf 'state=%s:%s\n' "$$" "$kept"`)
	matches := shellState.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		t.Fatal("guest shell state not printed")
	}
	return matches[len(matches)-1][1]
}
