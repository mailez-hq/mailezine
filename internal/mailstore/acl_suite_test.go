// Shared RFC 4314 ACL suite, executed against every MailboxStore backend
// that implements ACLStore (KV on all platforms, maildir on unix).
package mailstore

import (
	"context"
	"testing"
)

func aclSuite(t *testing.T, newStore func(*testing.T) MailboxStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("default empty", func(t *testing.T) {
		s := newStore(t)
		acl, err := s.(ACLStore).GetACL(ctx, "alice@example.com", "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if len(acl) != 0 {
			t.Fatalf("default ACL = %v, want empty", acl)
		}
	})

	t.Run("set get delete", func(t *testing.T) {
		s := newStore(t)
		astore := s.(ACLStore)
		if err := astore.SetACL(ctx, "alice@example.com", "INBOX", "bob@example.com", "lrswip"); err != nil {
			t.Fatal(err)
		}
		if err := astore.SetACL(ctx, "alice@example.com", "INBOX", "anyone", "lrs"); err != nil {
			t.Fatal(err)
		}
		acl, err := astore.GetACL(ctx, "alice@example.com", "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if acl["bob@example.com"] != "lrswip" || acl["anyone"] != "lrs" {
			t.Fatalf("ACL = %v", acl)
		}
		if err := astore.SetACL(ctx, "alice@example.com", "INBOX", "bob@example.com", ""); err != nil {
			t.Fatal(err)
		}
		acl, err = astore.GetACL(ctx, "alice@example.com", "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := acl["bob@example.com"]; ok {
			t.Fatalf("bob still present: %v", acl)
		}
		if err := astore.DeleteACL(ctx, "alice@example.com", "INBOX", "anyone"); err != nil {
			t.Fatal(err)
		}
		acl, err = astore.GetACL(ctx, "alice@example.com", "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if len(acl) != 0 {
			t.Fatalf("ACL after delete = %v", acl)
		}
	})

	t.Run("mailbox scoped", func(t *testing.T) {
		s := newStore(t)
		astore := s.(ACLStore)
		if _, err := s.CreateMailbox(ctx, "alice@example.com", "Shared"); err != nil {
			t.Fatal(err)
		}
		if err := astore.SetACL(ctx, "alice@example.com", "Shared", "bob@example.com", "lr"); err != nil {
			t.Fatal(err)
		}
		other, err := astore.GetACL(ctx, "alice@example.com", "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if len(other) != 0 {
			t.Fatalf("INBOX leaked ACL: %v", other)
		}
	})

	t.Run("rename carries acl", func(t *testing.T) {
		s := newStore(t)
		astore := s.(ACLStore)
		if _, err := s.CreateMailbox(ctx, "alice@example.com", "Old"); err != nil {
			t.Fatal(err)
		}
		if err := astore.SetACL(ctx, "alice@example.com", "Old", "bob@example.com", "lr"); err != nil {
			t.Fatal(err)
		}
		if err := s.RenameMailbox(ctx, "alice@example.com", "Old", "New"); err != nil {
			t.Fatal(err)
		}
		acl, err := astore.GetACL(ctx, "alice@example.com", "New")
		if err != nil {
			t.Fatal(err)
		}
		if acl["bob@example.com"] != "lr" {
			t.Fatalf("ACL lost on rename: %v", acl)
		}
	})
}
