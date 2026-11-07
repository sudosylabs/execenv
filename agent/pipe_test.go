package agent_test

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
)

// The sandbox boundary carries the host link on the agent's stdio.
func TestServeOverPipeLink(t *testing.T) {
	hostR, guestW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	guestR, hostW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- agent.Serve(ctx, agent.NewPipeConn(guestR, guestW), agent.Config{Home: t.TempDir(), Scratch: t.TempDir()})
	}()
	cli := agent.NewClient(agent.NewPipeConn(hostR, hostW))
	if err := cli.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cli.ReplaceTree(t.Context(), execenv.Tree{{Path: "a.txt", Kind: execenv.KindFile, Version: "v1", Data: []byte("hi")}}); err != nil {
		t.Fatal(err)
	}
	body, err := cli.Open(t.Context(), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "hi" {
		t.Fatalf("Open = %q", got)
	}
	_ = cli.Close()
	cancel()
	<-done
}
