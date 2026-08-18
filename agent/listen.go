package agent

import (
	"context"
	"net"

	"github.com/sudosylabs/execenv"
)

// LinkPort is the guest-side port the host dials after Ensure. The
// transport under that port is not part of the public type surface.
const LinkPort = 5000

// ListenAndServe accepts host connections on ln. Each connection is a
// new Serve; Home on disk is the durable state across hangups.
func ListenAndServe(ctx context.Context, ln net.Listener, cfg Config) error {
	if cfg.Home == "" {
		return execenv.Error("agent", execenv.ErrInvalid)
	}
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
		err = Serve(ctx, conn, cfg)
		_ = conn.Close()
		if err != nil && ctx.Err() == nil {
			return err
		}
	}
}
