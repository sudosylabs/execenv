package remote_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func TestRemoteSemanticReplayAndImmutableStreamingContent(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	first, second := serveInner(t, inner), serveInner(t, inner)
	spec := execenv.Spec{ID: "semantic", Image: "default"}
	env, err := first.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer env.Revoke(t.Context())
	other, err := second.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	one, two := env.(execenv.SemanticEnv), other.(execenv.SemanticEnv)
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: one.Epoch(), FenceRevision: 1}
	if _, err := one.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := one.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init"}); err != nil {
		t.Fatal(err)
	}
	captured := bytes.Repeat([]byte("capture-time content\n"), 20000)
	if err := inner.WriteGuest(t.Context(), env.ID(), "file", captured); err != nil {
		t.Fatal(err)
	}
	page, err := one.Observe(t.Context(), fence, 0, 128)
	if err != nil || len(page.Observations) != 1 {
		t.Fatalf("observe: %+v %v", page, err)
	}
	if err := inner.WriteGuest(t.Context(), env.ID(), "file", []byte("later content")); err != nil {
		t.Fatal(err)
	}
	replay, err := two.Observe(t.Context(), fence, 0, 1)
	if err != nil || len(replay.Observations) != 1 || *replay.Observations[0].Content != *page.Observations[0].Content {
		t.Fatalf("reconnect replay: %+v %v", replay, err)
	}
	reader, err := two.OpenObservationContent(t.Context(), fence, *page.Observations[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(body, captured) {
		t.Fatalf("streamed immutable body differs: %v %v", err, closeErr)
	}
	upload, err := two.UploadProjectionContent(t.Context(), fence, bytes.NewReader(captured))
	if err != nil {
		t.Fatal(err)
	}
	accepted := execenv.ProjectionMutation{JournalCursor: 1, EntryID: "accepted-guest", Kind: execenv.ProjectionFile, Operation: execenv.ProjectionCreate, Path: "file", ResultingContentVersion: "v1", Content: &upload}
	binding := execenv.ObservationProjection{Fence: fence, HostSequence: 1, Mutation: accepted}
	if err := two.ConfirmObservationProjection(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err := two.AcknowledgeObservations(t.Context(), fence, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := one.ApplyProjection(t.Context(), execenv.ProjectionApply{Fence: fence, MutationID: "accepted-outcome", ThroughWorkspaceCursor: 1, ExpectedHostCursor: 2, Mutations: []execenv.ProjectionMutation{accepted}}); err != nil {
		t.Fatal(err)
	}
	if err := one.ConfirmObservationProjection(t.Context(), binding); err != nil {
		t.Fatalf("confirmation retry after acknowledgement: %v", err)
	}
	live, err := env.Open(t.Context(), "file")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := io.ReadAll(live)
	_ = live.Close()
	if err != nil || string(latest) != "later content" {
		t.Fatal("confirmed echo overwrote later guest content")
	}
	if _, err := one.OpenObservationContent(t.Context(), fence, *page.Observations[0].Content); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("acknowledged content accessible: %v", err)
	}
	if _, err := one.Observe(t.Context(), fence, 0, 128); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("retired prefix silently skipped: %v", err)
	}
	page, err = one.Observe(t.Context(), fence, 1, 128)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].HostSequence != 2 {
		t.Fatalf("remaining event: %+v %v", page, err)
	}
	frozen := fence
	frozen.FenceRevision++
	if _, err := two.Control(t.Context(), execenv.ControlRequest{Fence: frozen, State: execenv.ControlFrozen}); err != nil {
		t.Fatal(err)
	}
	if _, err := one.OpenObservationContent(t.Context(), fence, *page.Observations[0].Content); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("stale fence read: %v", err)
	}
}
