package dkim

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	gdkim "github.com/emersion/go-msgauth/dkim"
	"mailezine/internal/testdns"
)

func TestSignAndVerify(t *testing.T) {
	// Generate the same key the backend would store (RSA-2048, PKCS#1 PEM).
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	txt := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pubDER)

	var hits atomic.Int64
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"data": map[string]any{
				"selectors": []map[string]any{
					{"domain": "example.com", "key": string(pemBytes), "selector": "dkim"},
				},
			},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(vault.Close)

	signer := NewSigner(vault.URL+"/stack/rspamd/vault", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: test\r\n\r\nbody\r\n")

	signed, err := signer.Sign(ctx, "alice@example.com", msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(signed, []byte("DKIM-Signature:")) {
		t.Fatalf("message not signed:\n%s", signed)
	}

	// Verify with go-msgauth, serving the public key via a fake DNS
	// resolver.
	resolver := &testdns.Resolver{TXT: map[string][]string{
		"dkim._domainkey.example.com.": {txt},
	}}
	results, err := gdkim.VerifyWithOptions(bytes.NewReader(signed), &gdkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			return resolver.LookupTXT(ctx, name)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Err != nil {
		for _, r := range results {
			t.Logf("result: domain=%s err=%v", r.Domain, r.Err)
		}
		t.Fatalf("signature did not verify: %+v", results)
	}

	// The key is cached: a second sign must not hit the vault.
	if _, err := signer.Sign(ctx, "alice@example.com", msg); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("vault hits = %d, want 1 (cached)", hits.Load())
	}
}

func TestSignWithoutKeyReturnsOriginal(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"selectors":[]}}`)
	}))
	t.Cleanup(vault.Close)

	signer := NewSigner(vault.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	msg := []byte("From: alice@nosign.example\r\nSubject: x\r\n\r\nbody\r\n")
	signed, err := signer.Sign(context.Background(), "alice@nosign.example", msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(signed) != string(msg) {
		t.Fatalf("message changed without key:\n%s", signed)
	}
}

func TestSignBadFrom(t *testing.T) {
	signer := NewSigner("http://127.0.0.1:1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	msg := []byte("Subject: x\r\n\r\nbody\r\n")
	signed, err := signer.Sign(context.Background(), "not-an-address", msg)
	if err != nil || string(signed) != string(msg) {
		t.Fatalf("bad from: signed=%q err=%v", signed, err)
	}
}
