//go:build linux || darwin

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/tree"
	"github.com/sudosylabs/execenv/internal/watchlog"
)

// This exercises the real guest wire and disk engine with quiescent test files.
// It deliberately does not claim to certify the kernel workload barrier.
func TestNativeJournalWireStreamsContentAndRetainsGuestEvents(t *testing.T) {
	client, home := journalWireFixture(t)
	ctx := t.Context()
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	initialBody := bytes.Repeat([]byte("initial body\n"), 20000)
	reference, err := client.UploadProjectionContent(ctx, fence, bytes.NewReader(initialBody))
	if err != nil {
		t.Fatal(err)
	}
	initial := execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{{EntryID: "file", Kind: execenv.ProjectionFile, Path: "file", ContentVersion: "v1", Content: &reference}}}
	receipt, err := client.InitializeProjection(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := client.InitializeProjection(ctx, initial)
	if err != nil || receipt != replay {
		t.Fatalf("initialization replay: %+v %v", replay, err)
	}
	if err := client.ReplaceTree(ctx, nil); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("legacy projection bypass: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "guest"), bytes.Repeat([]byte("captured\n"), 20000), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := client.Observe(ctx, fence, 0, 128)
	if err != nil || len(page.Observations) != 1 {
		t.Fatalf("guest observation: %+v %v", page, err)
	}
	if err := os.WriteFile(filepath.Join(home, "guest"), []byte("later"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := client.OpenObservationContent(ctx, fence, *page.Observations[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(body, bytes.Repeat([]byte("captured\n"), 20000)) {
		t.Fatalf("immutable streamed content: %v", err)
	}
	ownRef, err := client.UploadProjectionContent(ctx, fence, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	accepted := execenv.ProjectionMutation{JournalCursor: 2, EntryID: "guest-entry", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionCreate, Path: "guest", ResultingContentVersion: "v1", Content: &ownRef}
	if err := client.ConfirmObservationProjection(ctx, execenv.ObservationProjection{Fence: fence, HostSequence: 1, Mutation: accepted}); err != nil {
		t.Fatal(err)
	}
	if err := client.AcknowledgeObservations(ctx, fence, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenObservationContent(ctx, fence, *page.Observations[0].Content); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("acknowledged content: %v", err)
	}
	reference, err = client.UploadProjectionContent(ctx, fence, bytes.NewBufferString("saved"))
	if err != nil {
		t.Fatal(err)
	}
	apply := execenv.ProjectionApply{Fence: fence, MutationID: "save", ThroughWorkspaceCursor: 1, ExpectedHostCursor: 1, Mutations: []execenv.ProjectionMutation{{JournalCursor: 1, EntryID: "file", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionWrite, Path: "file", ExpectedContentVersion: "v1", ResultingContentVersion: "v2", Content: &reference}}}
	if _, err := client.ApplyProjection(ctx, apply); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("unobserved second guest write: %v", err)
	}
	page, err = client.Observe(ctx, fence, 1, 128)
	if err != nil || page.HostCursor != 2 {
		t.Fatalf("second write lost: %+v %v", page, err)
	}
	apply.ExpectedHostCursor = page.HostCursor
	if _, err := client.ApplyProjection(ctx, apply); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(filepath.Join(home, "file"))
	if err != nil || string(body) != "saved" {
		t.Fatalf("save: %q %v", body, err)
	}
	if _, err := client.ApplyProjection(ctx, execenv.ProjectionApply{Fence: fence, MutationID: "guest-outcome", FromWorkspaceCursor: 1, ThroughWorkspaceCursor: 2, ExpectedHostCursor: 2, Mutations: []execenv.ProjectionMutation{accepted}}); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(filepath.Join(home, "guest"))
	if err != nil || string(live) != "later" {
		t.Fatal("native wire echo overwrote later guest bytes")
	}
	page, err = client.Observe(ctx, fence, 2, 128)
	if err != nil || len(page.Observations) != 0 {
		t.Fatalf("projection echo: %+v %v", page, err)
	}
}

func journalWireFixture(t *testing.T) (*Client, string) {
	t.Helper()
	home := t.TempDir()
	hostConn, guestConn := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	server := &server{home: home, sess: newSession(guestConn), projected: make(tree.Snapshot), log: watchlog.New(watchlog.DefaultCapacity), journal: newDiskJournal(home, &testWriteBarrier{}, nil)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.shutdown()
		defer server.journal.close()
		_ = server.serveFrames(ctx)
	}()
	client := NewClient(hostConn)
	t.Cleanup(func() { cancel(); _ = client.Close(); _ = guestConn.Close(); <-done })
	if err := client.Verify(ctx); err != nil || !client.JournalEnabled() {
		t.Fatalf("guest capability handshake: %v", err)
	}
	return client, home
}
func TestNativeJournalWireInitializesLargeSnapshot(t *testing.T) {
	client, home := journalWireFixture(t)
	request := execenv.ProjectionInitialization{Fence: execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}, MutationID: "initial"}
	path := ""
	for i := 0; i < 3; i++ {
		if path != "" {
			path += "/"
		}
		path += strings.Repeat("&", execenv.MaxSegmentBytes)
		request.Entries = append(request.Entries, execenv.ProjectionEntry{EntryID: fmt.Sprintf("parent-%d", i), Kind: execenv.ProjectionDirectory, Path: path})
	}
	for i := len(request.Entries); i < execenv.MaxTreeEntries; i++ {
		request.Entries = append(request.Entries, execenv.ProjectionEntry{EntryID: fmt.Sprintf("entry-%d", i), Kind: execenv.ProjectionDirectory, Path: fmt.Sprintf("%s/%04d", path, i)})
	}
	raw, _ := json.Marshal(request)
	if len(raw) <= execenv.MaxProjectionMetadataBytes {
		t.Fatal("fixture did not exceed incremental limit")
	}
	receipt, err := client.InitializeProjection(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := client.InitializeProjection(t.Context(), request)
	if err != nil || replay != receipt {
		t.Fatalf("large initialization replay: %v", err)
	}
	if receipt.AppliedWorkspaceCursor != 0 {
		t.Fatal("initialization invented a cursor")
	}
	if _, err := os.Stat(filepath.Join(home, request.Entries[len(request.Entries)-1].Path)); err != nil {
		t.Fatal(err)
	}
}
