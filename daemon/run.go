package daemon

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"os"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
	"github.com/sudosylabs/execenv/remote"
)

// Run constructs the configured adapter, listens, then serves the existing
// remote contract. It does not invent a second protocol. The process stays
// inert (no grants) until Listen succeeds.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	inner, err := newAdapter(cfg)
	if err != nil {
		return err
	}
	ln, err := netListen(cfg.Listen)
	if err != nil {
		return err
	}
	return serveListener(ctx, ln, inner, cfg, logger)
}

func netListen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, execenv.Error("listen", err)
	}
	return ln, nil
}

func serveListener(ctx context.Context, ln net.Listener, inner execenv.Host, cfg Config, logger *slog.Logger) error {
	if cfg.security() == remote.SecurityTLS {
		if err := execenv.RequireIsolated(inner); err != nil {
			_ = ln.Close()
			return err
		}
	}
	// Log after bind so a failed listen cannot look like a running host.
	logger.Info("execenv listening", "detail", cfg.redacted())
	server := remote.ServerConfig{
		Security: cfg.security(),
		Token:    []byte(cfg.Token),
	}
	if cfg.security() == remote.SecurityTLS {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			_ = ln.Close()
			return execenv.Error("tls", execenv.ErrInvalid)
		}
		server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	}
	return remote.Serve(ctx, ln, inner, server)
}

// Main is the command entry: load path, then Run until ctx ends.
func Main(ctx context.Context, configPath string) error {
	cfg, err := Load(configPath)
	if err != nil {
		return err
	}
	return Run(ctx, cfg, nil)
}

func newAdapter(cfg Config) (execenv.Host, error) {
	switch cfg.Adapter {
	case adapterMemory:
		return memory.New(memory.Config{
			Images: cfg.imageIDs(),
			Slots:  cfg.Slots,
		})
	case adapterIsolated:
		// Ticket 05 supplies the isolated adapter. Fail closed until then
		// so production cannot silently start without isolation.
		return nil, execenv.Error("adapter", execenv.ErrUnavailable)
	default:
		return nil, execenv.Error("adapter", execenv.ErrInvalid)
	}
}


