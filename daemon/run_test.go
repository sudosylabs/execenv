package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
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

func TestLoadTLSRefusesMemory(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "tls",
		"adapter": "memory",
		"tls_cert": "cert.pem",
		"tls_key": "key.pem",
		"images": [{"id": "default"}],
		"slots": 1
	}`))
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("Load() error = %v, want ErrInvalid", err)
	}
}

func TestServeTLSRefusesMemory(t *testing.T) {
	t.Parallel()
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := netListen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	err = serveListener(t.Context(), ln, inner, Config{
		Listen:   "127.0.0.1:0",
		Token:    "secret",
		Security: securityTLS,
		Adapter:  adapterIsolated,
		TLSCert:  "cert.pem",
		TLSKey:   "key.pem",
	}, discardLogger())
	if !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("serveListener() error = %v, want ErrUnavailable", err)
	}
}

func TestOrdinaryLogsOmitSecrets(t *testing.T) {
	const (
		token = "super-secret-token"
		tree  = "tree-body-SECRET"
		pty   = "pty-octet-SECRET"
	)
	var mu sync.Mutex
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(lockedWriter{mu: &mu, w: &buf}, nil))
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "super-secret-token",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [{"id": "default"}, {"id": "alt"}],
		"slots": 2
	}`))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := listenAndRunLogger(t, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	client, err := remote.New(remote.Config{
		Address:  addr,
		Security: remote.SecurityInsecureLocal,
		Token:    []byte(token),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	env, err := client.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.ReplaceTree(t.Context(), execenv.Tree{{
		Path:    "secret.txt",
		Kind:    execenv.KindFile,
		Version: "v1",
		Data:    []byte(tree),
	}}); err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte(pty)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	for _, secret := range []string{token, tree, pty} {
		if strings.Contains(logged, secret) {
			t.Fatalf("ordinary log leaked %q: %s", secret, logged)
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
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
	return addr, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
