package auth

import (
	"context"
	"testing"
)

func TestDevAuthenticate(t *testing.T) {
	d := NewDev(map[string]string{"alice@example.com": "s3cret"})
	ctx := context.Background()
	ok, err := d.Authenticate(ctx, "alice@example.com", "s3cret", Options{Protocol: "imap"})
	if err != nil || !ok {
		t.Fatalf("valid password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = d.Authenticate(ctx, "alice@example.com", "wrong", Options{})
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
	ok, err = d.Authenticate(ctx, "nobody@example.com", "s3cret", Options{})
	if err != nil || ok {
		t.Fatalf("unknown user accepted: ok=%v err=%v", ok, err)
	}
}
