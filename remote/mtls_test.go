package remote_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
	"github.com/sudosylabs/execenv/remote"
)

func TestMutualTLSAuthenticatesWithoutToken(t *testing.T) {
	ca, caKey := certificateAuthority(t)
	serverCert := issuedCertificate(t, ca, caKey, true)
	clientCert := issuedCertificate(t, ca, caKey, false)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = remote.Serve(ctx, ln, inner, remote.ServerConfig{
			Security: remote.SecurityTLS,
			TLS: &tls.Config{
				Certificates: []tls.Certificate{serverCert},
				ClientCAs:    pool,
				ClientAuth:   tls.RequireAndVerifyClientCert,
				MinVersion:   tls.VersionTLS13,
			},
		})
	}()
	client, err := remote.New(remote.Config{
		Address:  ln.Addr().String(),
		Security: remote.SecurityTLS,
		TLS: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{clientCert},
			ServerName:   "execenv.test",
			MinVersion:   tls.VersionTLS13,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func certificateAuthority(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "execenv test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func issuedCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	usage := x509.ExtKeyUsageClientAuth
	name := "client"
	if server {
		usage = x509.ExtKeyUsageServerAuth
		name = "execenv.test"
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(int64(2 + len(name))),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
