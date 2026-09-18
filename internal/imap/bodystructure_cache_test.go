package imap

import (
	"context"
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

// stripsCachedMetadata hides the payloads the store caches at delivery, so a
// fetch against it has to read the blob and walk the message — the behaviour
// the cached path replaces.
type stripsCachedMetadata struct{ mailstore.MailboxStore }

func (s *stripsCachedMetadata) ListMessages(ctx context.Context, account, mailbox string) ([]*mailstore.Message, error) {
	msgs, err := s.MailboxStore.ListMessages(ctx, account, mailbox)
	for _, m := range msgs {
		m.Head, m.Body = nil, nil
	}
	return msgs, err
}

// The list path reports BODY from the structure cached at delivery instead of
// reading every message. Both routes must produce the same structure, for
// every MIME shape that matters — this is the wire-format equivalence the
// cached payload can silently break.
func TestStoredBodyStructureMatchesTheWalk(t *testing.T) {
	kv := mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	cs := &countingStore{MailboxStore: kv}
	cached := startTestServerWith(t, cs)
	walked := startTestServerWith(t, &stripsCachedMetadata{MailboxStore: kv})

	corpus := map[string]string{
		"simple": "From: a@example.com\r\nSubject: simple\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n",
		"alternative": "From: a@example.com\r\nSubject: alt\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: multipart/alternative; boundary=b1\r\n\r\n" +
			"--b1\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\nplain part\r\n" +
			"--b1\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>html=20part</p>\r\n" +
			"--b1--\r\n",
		"attachment": "From: a@example.com\r\nSubject: att\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=b2\r\n\r\n" +
			"--b2\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
			"--b2\r\nContent-Type: application/pdf; name*=utf-8''%E6%8A%A5%E5%91%8A.pdf\r\n" +
			"Content-Disposition: attachment; filename*=utf-8''%E6%8A%A5%E5%91%8A.pdf\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\nAAECAwQ=\r\n" +
			"--b2--\r\n",
		"nested": "From: a@example.com\r\nSubject: nested\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
			"--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
			"--inner\r\nContent-Type: text/plain\r\n\r\ninner plain\r\n" +
			"--inner\r\nContent-Type: text/html\r\n\r\n<i>inner html</i>\r\n" +
			"--inner--\r\n" +
			"--outer\r\nContent-Type: image/png; name=pixel.png\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw0KGgo=\r\n" +
			"--outer--\r\n",
		"message_rfc822": "From: a@example.com\r\nSubject: fwd\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=b3\r\n\r\n" +
			"--b3\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
			"--b3\r\nContent-Type: message/rfc822\r\n\r\n" +
			"From: b@example.com\r\nSubject: original\r\nContent-Type: text/plain\r\n\r\noriginal body\r\n" +
			"--b3--\r\n",
	}
	order := []string{"simple", "alternative", "attachment", "nested", "message_rfc822"}
	for _, name := range order {
		appendMessage(t, cached, "INBOX", corpus[name], nil)
	}

	for _, c := range []*imapclient.Client{cached, walked} {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	// The extended (BODYSTRUCTURE) form, which is what go-imap clients ask for
	// and the one the store caches.
	opts := &imap.FetchOptions{
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}
	set := imap.SeqSet{imap.SeqRange{Start: 1, Stop: uint32(len(order))}}

	before := int(cs.opened.Load())
	fromStore, err := cached.Fetch(set, opts).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if int(cs.opened.Load()) != before {
		t.Fatalf("body structure from the store opened %d blob(s), want 0", int(cs.opened.Load())-before)
	}
	fromWalk, err := walked.Fetch(set, opts).Collect()
	if err != nil {
		t.Fatal(err)
	}

	if len(fromStore) != len(order) || len(fromWalk) != len(order) {
		t.Fatalf("fetch sizes: store=%d walk=%d", len(fromStore), len(fromWalk))
	}
	for i, name := range order {
		if !reflect.DeepEqual(fromStore[i].BodyStructure, fromWalk[i].BodyStructure) {
			t.Errorf("%s: cached structure differs from the walked one:\nstore %#v\nwalk  %#v",
				name, fromStore[i].BodyStructure, fromWalk[i].BodyStructure)
		}
	}
}
