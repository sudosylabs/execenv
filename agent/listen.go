package agent

import (
	"context"
	"net"
	"sync"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/guestlink"
)

// LinkPort is the guest-side port the host dials after Ensure. The
// transport under that port is not part of the public type surface.
const LinkPort = 5000

// ListenGuest binds the guest-side link the host dials after Ensure. Off
// a guest it is ErrUnavailable; tests use a Unix socket via ListenAndServe.
func ListenGuest() (net.Listener, error) { return guestlink.Listen(LinkPort) }

// ListenAndServe accepts host links on ln for one helper. The newest link
// is the one served; a link that ends is a hangup, and the next one finds
// the workload, its processes and the process log as they were. The
// helper ends with ctx.
func ListenAndServe(ctx context.Context, ln net.Listener, cfg Config) error {
	a, err := New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	var links sync.WaitGroup
	defer func() {
		cancel()
		links.Wait()
		a.Close()
	}()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return execenv.Error("agent", err)
		}
		links.Go(func() {
			_ = a.Serve(ctx, conn)
			_ = conn.Close()
		})
	}
}
