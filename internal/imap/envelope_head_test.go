package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

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
	before := cs.opened
	msgs := fetch()
	if cs.opened != before {
		t.Fatalf("envelope fetch opened %d message blob(s), want 0", cs.opened-before)
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
