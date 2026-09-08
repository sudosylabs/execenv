//go:build isolation

package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// This path requires the installed daemon, TLS remote client and a real guest
// containing the current agent, kernel workload hierarchy and isolation hardware.
func TestCertifyRemoteJournalPreservesLiveWork(t *testing.T) {
	client := startCertifyHost(t, true)
	if !client.Capabilities().Isolated || !client.Capabilities().JournalProjection || !client.Capabilities().SemanticObservations {
		t.Fatal("native journal contract unavailable")
	}
	env, err := client.Ensure(t.Context(), execenv.Spec{ID: "journal-certify", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Revoke(context.Background()) })
	journal, ok := env.(execenv.SemanticEnv)
	if !ok {
		t.Fatal("advertised native contract missing")
	}
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
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if _, err := term.Write([]byte("kept=survives; printf '%s:%s\\n' \"$$\" \"$kept\" > before\n")); err != nil {
		t.Fatal(err)
	}
	before := waitCertifiedFile(t, env, "before", nil)
	page, err := journal.Observe(t.Context(), fence, 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	cursor := page.HostCursor
	if _, err := term.Write([]byte("printf 'guest-first' > guest\n")); err != nil {
		t.Fatal(err)
	}
	waitCertifiedFile(t, env, "guest", []byte("guest-first"))
	reference, err = journal.UploadProjectionContent(t.Context(), fence, bytes.NewBufferString("saved\n"))
	if err != nil {
		t.Fatal(err)
	}
	save := execenv.ProjectionApply{Fence: fence, MutationID: "ordinary-save", FromWorkspaceCursor: 10, ThroughWorkspaceCursor: 11, ExpectedHostCursor: cursor, Mutations: []execenv.ProjectionMutation{{JournalCursor: 11, EntryID: "file", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "dir/file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &reference}}}
	if _, err := journal.ApplyProjection(t.Context(), save); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("save absorbed unseen guest change: %v", err)
	}
	page, err = journal.Observe(t.Context(), fence, cursor, 128)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Operation != execenv.ObservationCreateFile || page.Observations[0].Path != "guest" {
		t.Fatalf("guest change not retained exactly once: %+v %v", page, err)
	}
	guest := page.Observations[0]
	save.ExpectedHostCursor = page.HostCursor
	if _, err := journal.ApplyProjection(t.Context(), save); err != nil {
		t.Fatal(err)
	}
	waitCertifiedFile(t, env, "dir/file", []byte("saved\n"))
	if _, err := term.Write([]byte("printf 'guest-later' > guest\n")); err != nil {
		t.Fatal(err)
	}
	waitCertifiedFile(t, env, "guest", []byte("guest-later"))
	reader, err := journal.OpenObservationContent(t.Context(), fence, *guest.Content)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(captured) != "guest-first" {
		t.Fatalf("observation used later live bytes: %q %v", captured, err)
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
	page, err = journal.Observe(t.Context(), fence, guest.HostSequence, 128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyProjection(t.Context(), execenv.ProjectionApply{Fence: fence, MutationID: "guest-outcome", FromWorkspaceCursor: 11, ThroughWorkspaceCursor: 12, ExpectedHostCursor: page.HostCursor, Mutations: []execenv.ProjectionMutation{accepted}}); err != nil {
		t.Fatal(err)
	}
	waitCertifiedFile(t, env, "guest", []byte("guest-later"))
	cursor = page.HostCursor
	if _, err := term.Write([]byte("mv dir moved\n")); err != nil {
		t.Fatal(err)
	}
	waitCertifiedFile(t, env, "moved/file", []byte("saved\n"))
	page, err = journal.Observe(t.Context(), fence, cursor, 128)
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
	if _, err := term.Write([]byte("printf '%s:%s\\n' \"$$\" \"$kept\" > after\n")); err != nil {
		t.Fatalf("same PTY lost: %v", err)
	}
	after := waitCertifiedFile(t, env, "after", before)
	if !bytes.Equal(before, after) || journal.Epoch() != fence.EnvironmentEpoch {
		t.Fatal("save/freeze replaced the healthy shell or epoch")
	}
}
func waitCertifiedFile(t *testing.T, env execenv.Env, name string, want []byte) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		reader, err := env.Open(ctx, name)
		if err == nil {
			body, readErr := io.ReadAll(reader)
			_ = reader.Close()
			if readErr == nil && (want == nil && len(body) > 0 || want != nil && bytes.Equal(body, want)) {
				return body
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("guest file %s did not reach expected content", name)
		case <-ticker.C:
		}
	}
}
