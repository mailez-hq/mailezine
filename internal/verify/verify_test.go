package verify

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mailezine/internal/dkim"
	"mailezine/internal/testdns"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// signedTestMessage returns a DKIM-signed message plus the DNS TXT record of
// its public key.
func signedTestMessage(t *testing.T) ([]byte, string) {
	t.Helper()
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

	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	signer := dkim.NewSigner(vault.URL, discardLogger())
	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: test\r\n\r\nbody\r\n")
	signed, err := signer.Sign(context.Background(), "alice@example.com", msg)
	if err != nil {
		t.Fatal(err)
	}
	return signed, txt
}

func newTestResolver(spfTXT, dkimTXT, dmarcTXT string) *testdns.Resolver {
	txt := map[string][]string{}
	if spfTXT != "" {
		txt["example.com."] = []string{spfTXT}
	}
	if dkimTXT != "" {
		txt["dkim._domainkey.example.com."] = []string{dkimTXT}
	}
	if dmarcTXT != "" {
		txt["_dmarc.example.com."] = []string{dmarcTXT}
	}
	return &testdns.Resolver{TXT: txt}
}

func TestVerifyPass(t *testing.T) {
	signed, dkimTXT := signedTestMessage(t)
	v := &Verifier{
		Logger:   discardLogger(),
		Resolver: newTestResolver("v=spf1 ip4:127.0.0.1 -all", dkimTXT, "v=DMARC1; p=reject"),
		Hostname: "mail.mailez.test",
	}
	header, err := v.Verify(context.Background(), net.ParseIP("127.0.0.1"), "alice@example.com", signed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"spf=pass", "dkim=pass", "dmarc=pass", "smtp.mailfrom=example.com"} {
		if !strings.Contains(header, want) {
			t.Fatalf("header missing %q:\n%s", want, header)
		}
	}
}

func TestVerifySPFFail(t *testing.T) {
	signed, dkimTXT := signedTestMessage(t)
	v := &Verifier{
		Logger:   discardLogger(),
		Resolver: newTestResolver("v=spf1 ip4:127.0.0.1 -all", dkimTXT, ""),
		Hostname: "mail.mailez.test",
	}
	header, err := v.Verify(context.Background(), net.ParseIP("203.0.113.9"), "alice@example.com", signed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(header, "spf=fail") {
		t.Fatalf("expected spf=fail:\n%s", header)
	}
	if !strings.Contains(header, "dkim=pass") {
		t.Fatalf("expected dkim=pass:\n%s", header)
	}
}

func TestVerifyNoSignature(t *testing.T) {
	v := &Verifier{
		Logger:   discardLogger(),
		Resolver: newTestResolver("v=spf1 ip4:127.0.0.1 -all", "", ""),
		Hostname: "mail.mailez.test",
	}
	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: x\r\n\r\nbody\r\n")
	header, err := v.Verify(context.Background(), net.ParseIP("127.0.0.1"), "alice@example.com", msg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(header, "dkim=none") {
		t.Fatalf("expected dkim=none:\n%s", header)
	}
	if !strings.Contains(header, "spf=pass") {
		t.Fatalf("expected spf=pass:\n%s", header)
	}
}
