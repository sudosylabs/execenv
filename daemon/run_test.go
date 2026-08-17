package daemon

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/remote"
)

func TestRunServesMemoryLocally(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [{"id": "default"}, {"id": "alt"}],
		"slots": 2,
		"network": "none"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := listenAndRun(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client, err := remote.New(remote.Config{
		Address:  ln,
		Security: remote.SecurityInsecureLocal,
		Token:    []byte("secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	report, err := client.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Usable {
		t.Fatal("Ready() Usable = false")
	}
	env, err := client.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if env.ID() != "grant-1" {
		t.Fatalf("ID() = %q", env.ID())
	}
}

func TestRedactedOmitsToken(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "super-secret-token",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [{"id": "default"}],
		"slots": 1
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg.redacted(), "super-secret-token") {
		t.Fatal("redacted config leaked token")
	}
}

func listenAndRun(t *testing.T, cfg Config) (string, error) {
	t.Helper()
	return listenAndRunLogger(t, cfg, discardLogger())
}

func listenAndRunLogger(t *testing.T, cfg Config, logger *slog.Logger) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Bind first so the test knows the real port when listen is :0.
	// Run() also listens; we use a wrapper that reports the address.
	addr, err := start(ctx, cfg, logger)
	return addr, err
}

func start(ctx context.Context, cfg Config, logger *slog.Logger) (string, error) {
	inner, err := newAdapter(cfg)
	if err != nil {
		return "", err
	}
	ln, err := netListen(cfg.Listen)
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	go func() {
		_ = serveListener(ctx, ln, inner, cfg, logger)
	}()
	// Give Accept a moment to start. The client retries on dial in practice;
	// here the listen socket already exists.
	time.Sleep(20 * time.Millisecond)
	return addr, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
