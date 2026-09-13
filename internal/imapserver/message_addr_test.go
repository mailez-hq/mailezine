package imapserver

import (
	"bufio"
	"strings"
	"testing"

	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

func hdr(t *testing.T, raw string) mail.Header {
	t.Helper()
	h, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return mail.Header{Header: gomessage.Header{Header: h}}
}

func TestParseAddressListGBKFrom(t *testing.T) {
	// Real-world 126.com header: GBK-encoded display name. The stock
	// parser blanks the whole From on this; we must decode and keep both
	// the name and the address.
	h := hdr(t, "From: =?GBK?B?pe2/qKXtsuw=?= <gushing@126.com>\r\n")
	got := parseAddressList(h, "From")
	if len(got) != 1 {
		t.Fatalf("addresses: %+v", got)
	}
	if got[0].Mailbox != "gushing" || got[0].Host != "126.com" {
		t.Fatalf("address: %+v", got[0])
	}
	if got[0].Name == "" {
		t.Fatalf("display name not decoded: %+v", got[0])
	}
	for _, r := range got[0].Name {
		if r > 0xFFFF {
			t.Fatalf("name not valid text: %q", got[0].Name)
		}
	}
	t.Logf("decoded name: %q", got[0].Name)
}

func TestParseAddressListBig5(t *testing.T) {
	h := hdr(t, "From: =?Big5?B?trRmraVZs8m2RLfalWk=?= <someone@example.com.tw>\r\n")
	got := parseAddressList(h, "From")
	if len(got) != 1 || got[0].Mailbox != "someone" || got[0].Host != "example.com.tw" {
		t.Fatalf("big5: %+v", got)
	}
}

func TestParseAddressListFallback(t *testing.T) {
	// Undecodable junk the strict parser rejects: the email must survive
	// via the regex fallback even if the name is lost.
	h := hdr(t, "From: \"Weird <Name>\" not-an-addr <a@b.example>, trailing garbage c@d.example\r\n")
	got := parseAddressList(h, "From")
	if len(got) == 0 {
		t.Fatal("fallback produced no addresses")
	}
	joined := got[0].Mailbox + "@" + got[0].Host
	if !strings.Contains(joined, "@") {
		t.Fatalf("bad fallback address: %+v", got)
	}
}

func TestParseAddressListPlain(t *testing.T) {
	h := hdr(t, "From: Alice <alice@example.com>\r\n")
	got := parseAddressList(h, "From")
	if len(got) != 1 || got[0].Name != "Alice" || got[0].Mailbox != "alice" {
		t.Fatalf("plain: %+v", got)
	}
}
