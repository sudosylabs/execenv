package remote_test

import (
	"crypto/tls"
	"errors"
	"net"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
	"github.com/sudosylabs/execenv/remote"
)

func TestServeRejectsUnsafeConfiguration(t *testing.T) {
	inner, err := memory.New(memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ip   net.IP
		cfg  remote.ServerConfig
		want error
	}{
		{
			name: "TLS requires isolation",
			ip:   net.IPv4(127, 0, 0, 1),
			cfg:  remote.ServerConfig{Security: remote.SecurityTLS, TLS: &tls.Config{}, Token: []byte("fixture")},
			want: execenv.ErrUnavailable,
		},
		{
			name: "cleartext requires loopback",
			ip:   net.IPv4zero,
			cfg:  remote.ServerConfig{Security: remote.SecurityInsecureLocal, Token: []byte("fixture")},
			want: execenv.ErrInvalid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := addressOnlyListener{addr: &net.TCPAddr{IP: tc.ip, Port: 1}}
			if err := remote.Serve(t.Context(), ln, inner, tc.cfg); !errors.Is(err, tc.want) {
				t.Fatalf("Serve() = %v, want %v", err, tc.want)
			}
		})
	}
}

type addressOnlyListener struct {
	net.Listener
	addr net.Addr
}

func (l addressOnlyListener) Addr() net.Addr { return l.addr }

func TestScopedRevokeRetryLeavesReplacement(t *testing.T) {
	host := startLoopback(t)
	spec := execenv.Spec{ID: "scoped", Image: "default"}
	old, err := host.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := old.Revoke(t.Context()); err != nil {
		t.Fatalf("repeated revoke: %v", err)
	}
	replacement, err := host.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Revoke(t.Context()); err != nil {
		t.Fatalf("retired epoch revoke: %v", err)
	}
	if _, err := replacement.ControlStatus(t.Context()); err != nil {
		t.Fatalf("replacement unavailable: %v", err)
	}
}
