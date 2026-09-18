package imap

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// The list path reads every row for its body structure and then reads the same
// rows again for their preview fragments. The second pass must come from the
// buffer memo: that duplicate blob read is ~440ms of a cold 50-row page.
func TestFetchReusesRawBufferForLaterSections(t *testing.T) {
	cs := &countingStore{MailboxStore: mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))}
	c := startTestServerWith(t, cs)
	body := "From: a@example.com\r\nSubject: preview reuse\r\n\r\nhello preview body\r\n"
	appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	set := imap.SeqSet{imap.SeqRange{Start: 1, Stop: 1}}

	rows, err := c.Fetch(set, &imap.FetchOptions{
		Envelope:      true,
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{},
		BodySection: []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierHeader, Peek: true},
		},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("row fetch returned %d messages", len(rows))
	}
	opens := int(cs.opened.Load())
	if opens == 0 {
		t.Fatal("the row fetch must read the message once (body structure needs it)")
	}

	preview, err := c.Fetch(set, &imap.FetchOptions{
		UID: true,
		BodySection: []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierText, Partial: &imap.SectionPartial{Offset: 0, Size: 64}, Peek: true},
		},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if int(cs.opened.Load()) != opens {
		t.Fatalf("the preview fetch re-read the blob: %d extra open(s)", int(cs.opened.Load())-opens)
	}
	if len(preview) != 1 || !strings.Contains(string(preview[0].BodySection[0].Bytes), "hello preview body") {
		t.Fatalf("preview fetch returned %d messages: %+v", len(preview), preview)
	}
}

// An envelope-only FETCH (exactly what the list's thread scan issues, 300
// messages at a time) must be answered from the cached header block instead of
// reading every message blob.
func TestFetchEnvelopeUsesCachedHeaderBlock(t *testing.T) {
	cs := &countingStore{MailboxStore: mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))}
	c := startTestServerWith(t, cs)
	// Deliberately awkward headers: an RFC 2047 encoded subject, a display
	// name, Cc, In-Reply-To and a Date — the envelope built from the stored
	// header block must equal the one built from the raw message.
	body := "From: A Sender <a@example.com>\r\n" +
		"To: B <b@example.com>\r\n" +
		"Cc: C <c@example.com>\r\n" +
		"Subject: =?UTF-8?B?5rWL6K+VIGhlYWQ=?=\r\n" +
		"Date: Wed, 10 Sep 2026 10:00:00 +0800\r\n" +
		"Message-ID: <h1@example.com>\r\n" +
		"In-Reply-To: <h0@example.com>\r\n" +
		"\r\nhello\r\n"
	appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	fetch := func() []*imapclient.FetchMessageBuffer {
		t.Helper()
		msgs, err := c.Fetch(imap.SeqSet{imap.SeqRange{Start: 1, Stop: 1}}, &imap.FetchOptions{
			Envelope: true,
			UID:      true,
		}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		return msgs
	}
	before := int(cs.opened.Load())
	msgs := fetch()
	if int(cs.opened.Load()) != before {
		t.Fatalf("envelope fetch opened %d message blob(s), want 0", int(cs.opened.Load())-before)
	}
	if len(msgs) != 1 || msgs[0].Envelope == nil {
		t.Fatalf("envelope = %+v", msgs)
	}
	// The cached-header envelope must match what the blob would produce.
	want := envelopeOf([]byte(body))
	got := msgs[0].Envelope
	if got.Subject != want.Subject || got.MessageID != want.MessageID ||
		len(got.InReplyTo) != len(want.InReplyTo) || len(got.From) != len(want.From) ||
		len(got.To) != len(want.To) || len(got.Cc) != len(want.Cc) ||
		!got.Date.Equal(want.Date) {
		t.Fatalf("cached-head envelope differs from the raw-message envelope:\ngot  %+v\nwant %+v", got, want)
	}
}
