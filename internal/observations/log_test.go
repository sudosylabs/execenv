package observations

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

func captured(body string) Captured {
	return Captured{Event: execenv.ExecutionObservation{Fence: execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}, BasedOnWorkspaceCursor: 7, Operation: execenv.ObservationWriteFile, NodeIdentity: "node", Kind: execenv.ProjectionFile, Path: "file", ExpectedContentVersion: "v1"}, Data: []byte(body)}
}
func TestRetainedContentSurvivesReplayUntilAcknowledged(t *testing.T) {
	log := New("grant", "epoch")
	one, two := captured("first"), captured("second")
	if err := log.Append([]Captured{one, two}); err != nil {
		t.Fatal(err)
	}
	one.Data[0] = 'X'
	page, err := log.Page(0, 128)
	if err != nil || len(page.Observations) != 2 {
		t.Fatalf("page: %+v %v", page, err)
	}
	for i, want := range []string{"first", "second"} {
		event := page.Observations[i]
		if event.HostSequence != uint64(i+1) || event.BasedOnWorkspaceCursor != 7 || event.ExpectedContentVersion != "v1" {
			t.Fatalf("lost original baseline: %+v", event)
		}
		body, err := log.Content(*event.Content)
		if err != nil || string(body) != want {
			t.Fatalf("immutable event: %q %v", body, err)
		}
		body[0] = 'X'
		again, err := log.Content(*event.Content)
		if err != nil || string(again) != want {
			t.Fatal("read bytes alias retention")
		}
	}
	reference := *page.Observations[0].Content
	page.Observations[0].Content.TransferID = "changed"
	replay, err := log.Page(0, 1)
	if err != nil || *replay.Observations[0].Content != reference {
		t.Fatal("page mutated retained envelope")
	}
	foreign := New("other", "epoch")
	if _, err := foreign.Content(reference); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("cross-grant reference: %v", err)
	}
	if err := log.Acknowledge(1); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Content(reference); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("acknowledged bytes retained: %v", err)
	}
	if _, err := log.Page(0, 1); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("missing prefix hidden: %v", err)
	}
	if err := log.Acknowledge(3); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("future acknowledgement: %v", err)
	}
	if err := log.Acknowledge(1); err != nil {
		t.Fatal("acknowledgement retry failed")
	}
	if log.bytes != int64(len("second")) {
		t.Fatal("incorrect retained byte accounting")
	}
}
func TestCaptureValidationIsAtomicAndOverflowRetainsEvidence(t *testing.T) {
	log := New("grant", "epoch")
	bad := captured("bad")
	bad.Event.Fence.EnvironmentEpoch = "other"
	if err := log.Append([]Captured{captured("valid"), bad}); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatal(err)
	}
	if log.cursor != 0 || len(log.events) != 0 || log.bytes != 0 {
		t.Fatal("partially appended invalid capture")
	}
	if err := log.Append([]Captured{captured("retained")}); err != nil {
		t.Fatal(err)
	}
	page, err := log.Page(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := *page.Observations[0].Content
	over := make([]Captured, maxRetainedEvents)
	if err := log.Append(over); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("unbounded retained events: %v", err)
	}
	body, err := log.Content(ref)
	if err != nil || !bytes.Equal(body, []byte("retained")) {
		t.Fatal("overflow discarded unacknowledged evidence")
	}
	if _, err := log.Page(0, 1); err != nil {
		t.Fatal("overflow made retained prefix inaccessible")
	}
	if _, err := log.Page(1, 1); !errors.Is(err, execenv.ErrLagged) {
		t.Fatal("overflow not reported at end of prefix")
	}
}
