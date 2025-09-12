package mailstore

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"mailezine/internal/store"
)

const headRaw = "From: a@example.com\r\nSubject: cached\r\nMessage-ID: <m1@example.com>\r\n\r\nbody\r\n"
const headWant = "From: a@example.com\r\nSubject: cached\r\nMessage-ID: <m1@example.com>\r\n\r\n"

// A delivered message carries its header block, so an envelope fetch later
// needs no blob read (that is what keeps the list's 300-message thread scan
// off the object store).
func TestDeliverCachesHeaderBlock(t *testing.T) {
	ctx := t.Context()
	kv := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	if _, err := kv.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte(headRaw)}); err != nil {
		t.Fatal(err)
	}
	msgs, err := kv.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || string(msgs[0].Head) != headWant {
		t.Fatalf("head = %q, want %q", msgs[0].Head, headWant)
	}

	// A message with no blank line has no usable header block, but the field
	// must still be written — an empty value is what stops Reindex from
	// reading that message again on every pass.
	if _, err := kv.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte("no body separator")}); err != nil {
		t.Fatal(err)
	}
	msgs, err = kv.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.UID == 2 && len(m.Head) != 0 {
			t.Fatalf("message without a blank line must not cache headers: %q", m.Head)
		}
	}
}

func TestHeaderBlockShapes(t *testing.T) {
	if got := string(headerBlock([]byte(headRaw))); got != headWant {
		t.Fatalf("crlf head = %q", got)
	}
	if got := string(headerBlock([]byte("Subject: lf\n\nbody"))); got != "Subject: lf\n\n" {
		t.Fatalf("lf head = %q", got)
	}
	if got := headerBlock([]byte("Subject: no separator")); got != nil {
		t.Fatalf("head without a blank line = %q, want nil", got)
	}
	// Headers past the cap are left to the blob path: a truncated block would
	// silently drop envelope fields.
	long := "Subject: x\r\n" + strings.Repeat("X-Long: y\r\n", maxStoredHeaderBytes/10) + "\r\nbody"
	if got := headerBlock([]byte(long)); got != nil {
		t.Fatalf("oversized head = %d bytes, want nil", len(got))
	}
}

// Delivery also caches the non-extended body structure, which is what a list
// row reports instead of reading the message.
func TestDeliverCachesBodyStructure(t *testing.T) {
	ctx := t.Context()
	kv := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	raw := "From: a@example.com\r\nSubject: bs\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nAAEC\r\n" +
		"--b--\r\n"
	if _, err := kv.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: []byte(raw)}); err != nil {
		t.Fatal(err)
	}
	msgs, err := kv.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d", len(msgs))
	}
	got := string(msgs[0].Body)
	if !strings.HasPrefix(got, `(("text" "plain"`) || !strings.Contains(got, `"mixed"`) {
		t.Fatalf("cached body structure = %q", got)
	}
	if !strings.Contains(got, `"octet-stream"`) {
		t.Fatalf("cached body structure lost the attachment part: %q", got)
	}
	// A message too large to parse on the delivery path stays on the blob.
	big := make([]byte, maxStructuredMessageBytes+1)
	copy(big, []byte("Subject: big\r\n\r\n"))
	if _, err := kv.Deliver(ctx, "alice@example.com", "INBOX", &Message{Data: big}); err != nil {
		t.Fatal(err)
	}
	msgs, err = kv.ListMessages(ctx, "alice@example.com", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.UID == 2 && len(m.Body) != 0 {
			t.Fatalf("oversized message must not cache a structure: %q", m.Body)
		}
	}
}

// Reindex is the migration path for messages written before the header block
// existed: it fills the field once, from the blob, and leaves it alone after.
func TestReindexBackfillsHeaderBlock(t *testing.T) {
	ctx := t.Context()
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	kv := NewKV(s)
	const acct = "alice@example.com"
	if _, err := kv.CreateMailbox(ctx, acct, "INBOX"); err != nil {
		t.Fatal(err)
	}
	acctID, err := s.AccountByEmail(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	mbID, err := kv.mailboxDocID(ctx, acctID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}

	// A legacy document: the same fields Deliver writes minus the header.
	blobID := "sha256-legacy"
	if _, err := s.PutBlob(ctx, blobID, int64(len(headRaw)), bytes.NewReader([]byte(headRaw))); err != nil {
		t.Fatal(err)
	}
	legacy := map[byte][]byte{
		fieldBlobID:   []byte(blobID),
		fieldMailbox:  []byte("INBOX"),
		fieldDate:     []byte(time.Now().UTC().Format(time.RFC3339)),
		fieldFrom:     []byte("a@example.com"),
		fieldSize:     beUint64(uint64(len(headRaw))),
		fieldKeywords: []byte(""),
	}
	if _, err := s.DeliverEmailBatch(ctx, acctID, []store.DeliverRequest{{
		MailboxID: mbID, Fields: legacy, BlobID: blobID, Size: int64(len(headRaw)),
	}}); err != nil {
		t.Fatal(err)
	}
	msgs, err := kv.ListMessages(ctx, acct, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].Head) != 0 {
		t.Fatalf("legacy message should have no cached head yet: %q", msgs[0].Head)
	}

	if _, err := kv.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, err = kv.ListMessages(ctx, acct, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if string(msgs[0].Head) != headWant {
		t.Fatalf("backfilled head = %q, want %q", msgs[0].Head, headWant)
	}
	if len(msgs[0].Body) == 0 || !strings.HasPrefix(string(msgs[0].Body), `("text" "plain"`) {
		t.Fatalf("backfilled body structure = %q", msgs[0].Body)
	}
	if n, err := kv.Reindex(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("second Reindex still rewrote %d entry/entries", n)
	}
}
