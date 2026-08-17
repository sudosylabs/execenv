package remote

import (
	"crypto/tls"
	"net"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
)

// Security selects how the client authenticates the host.
type Security uint8

const (
	// SecurityTLS is the production mode. New requires a TLS config and a
	// token (or a client certificate on the TLS config).
	SecurityTLS Security = iota
	// SecurityInsecureLocal is cleartext for loopback tests only.
	SecurityInsecureLocal
)

// Config dials one host. Token is never written to logs.
type Config struct {
	Address    string
	ServerName string
	TLS        *tls.Config
	Security   Security
	Token      []byte
	Timeout    time.Duration
}

// ServerConfig is the host side of Config.
type ServerConfig struct {
	Security Security
	TLS      *tls.Config
	Token    []byte
}

func validateClient(cfg Config) error {
	if cfg.Address == "" {
		return execenv.Error("dial", execenv.ErrInvalid)
	}
	switch cfg.Security {
	case SecurityTLS:
		if cfg.TLS == nil {
			return execenv.Error("dial", execenv.ErrInvalid)
		}
		if len(cfg.Token) == 0 && len(cfg.TLS.Certificates) == 0 {
			return execenv.Error("dial", execenv.ErrInvalid)
		}
	case SecurityInsecureLocal:
		if !isLoopback(cfg.Address) {
			return execenv.Error("dial", execenv.ErrInvalid)
		}
	default:
		return execenv.Error("dial", execenv.ErrInvalid)
	}
	return nil
}

func validateServer(cfg ServerConfig) error {
	switch cfg.Security {
	case SecurityTLS:
		if cfg.TLS == nil || len(cfg.Token) == 0 {
			return execenv.Error("serve", execenv.ErrInvalid)
		}
	case SecurityInsecureLocal:
	default:
		return execenv.Error("serve", execenv.ErrInvalid)
	}
	return nil
}

func isLoopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsLoopback()
	}
	// Reject hostnames other than localhost so "insecure local" cannot
	// accidentally target a public name.
	return strings.EqualFold(host, "localhost")
}

func timeoutOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Second
	}
	return d
}
