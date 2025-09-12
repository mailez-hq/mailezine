package mailstore

import (
	"testing"

	"mailezine/internal/store"
)

// MailboxMeta exists so IMAP SELECT can skip the mailbox-wide walk
// MailboxStatus does for its counters. It must report exactly the identity
// fields of the status — and leave the counters it does not compute at zero.
func TestMailboxMetaMatchesStatusIdentity(t *testing.T) {
	ctx := t.Context()
	kv := NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	const acct = "alice@example.com"
	if _, err := kv.CreateMailbox(ctx, acct, "INBOX"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := kv.Deliver(ctx, acct, "INBOX", &Message{Data: []byte("Subject: m\r\n\r\nbody")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := kv.SetFlags(ctx, acct, "INBOX", 2, []string{"\\Seen"}); err != nil {
		t.Fatal(err)
	}

	meta, err := kv.MailboxMeta(ctx, acct, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	st, err := kv.MailboxStatus(ctx, acct, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != st.Name || meta.UIDValidity != st.UIDValidity || meta.UIDNext != st.UIDNext ||
		meta.Subscribed != st.Subscribed || meta.HighestModSeq != st.HighestModSeq {
		t.Fatalf("meta identity differs from status:\n%+v\n%+v", meta, st)
	}
	if meta.NumMessages != 0 || meta.NumUnseen != 0 || meta.NumDeleted != 0 || meta.Size != 0 {
		t.Fatalf("meta must not report counters it never computed: %+v", meta)
	}
	if st.NumMessages != 3 || st.NumUnseen != 2 {
		t.Fatalf("status counters wrong: %+v", st)
	}
	if _, err := kv.MailboxMeta(ctx, acct, "Nope"); err == nil {
		t.Fatal("unknown mailbox must report an error, not a zero status")
	}
}
