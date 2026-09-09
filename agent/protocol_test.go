package agent

import (
	"errors"
	"net"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestVerifyRejectsHelperWithOldInitializationLimit(t *testing.T) {
	clientConn, helperConn := net.Pipe()
	client := NewClient(clientConn)
	t.Cleanup(func() { _ = client.Close(); _ = helperConn.Close() })
	helper := newSession(helperConn)
	done := make(chan error, 1)
	go func() {
		request, err := helper.Recv()
		if err != nil {
			done <- err
			return
		}
		extra, err := encodeExtra(helloResult{Version: 3, Journal: true})
		if err == nil {
			err = helper.Send(frame{Kind: kindResponse, Seq: request.Seq, Extra: extra})
		}
		done <- err
	}()
	if err := client.Verify(t.Context()); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("old helper accepted: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.JournalEnabled() {
		t.Fatal("old helper advertised current journal capability")
	}
}
