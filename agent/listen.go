package agent

import (
	"context"
	"net"

	"github.com/sudosylabs/execenv"
)

// LinkPort is the guest-side port the host dials after Ensure. The
// transport under that port is not part of the public type surface.
const LinkPort = 5000

// ListenAndServe accepts host links on ln for one helper. The newest link
// is the one served; a link that ends is a hangup, and the next one finds
// the workload, its processes and the process log as they were. The
// helper ends with ctx.
func ListenAndServe(ctx context.Context, ln net.Listener, cfg Config) error {
	a, err := New(cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return execenv.Error("agent", err)
		}
		go func() {
			_ = a.Serve(ctx, conn)
			_ = conn.Close()
		}()
	}
}
