package imap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func TestIMAPStartTLS(t *testing.T) {
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true},
		},
		Domains: []string{"example.com"},
	})
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	srv := New(&Server{
		Store:     mailstore.NewKV(s),
		Auth:      auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory: dir,
		TLSConfig: selfSignedTLS(t),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	client, err := imapclient.DialStartTLS(ln.Addr().String(), &imapclient.Options{
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatalf("DialStartTLS: %v", err)
	}
	t.Cleanup(func() { _ = client.Logout().Wait() })
	if err := client.Login("alice@example.com", "s3cret").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
}

func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mail.mailez.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"mail.mailez.test", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	if cert.Leaf, err = x509.ParseCertificate(der); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}
