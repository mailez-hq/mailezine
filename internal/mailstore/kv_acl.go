// RFC 4314 ACL persistence on the KV backend: the ACL lives in the mailbox
// document (mbFieldACL) so it renames and deletes atomically with the
// mailbox itself.
package mailstore

import (
	"context"
	"encoding/json"

	"mailezine/internal/store"
)

var _ ACLStore = (*KV)(nil)

// GetACL returns the ACL entries of one mailbox (identifier → rights).
func (k *KV) GetACL(ctx context.Context, account, mailbox string) (map[string]string, error) {
	acctID, mbID, err := k.aclDoc(ctx, account, mailbox)
	if err != nil {
		return nil, err
	}
	fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionMailbox, mbID)
	if err != nil {
		return nil, err
	}
	acl := map[string]string{}
	if len(fields[mbFieldACL]) > 0 {
		if err := json.Unmarshal(fields[mbFieldACL], &acl); err != nil {
			return nil, err
		}
	}
	return acl, nil
}

// SetACL replaces the rights of identifier, or removes the entry when
// rights is empty.
func (k *KV) SetACL(ctx context.Context, account, mailbox, identifier, rights string) error {
	cur, err := k.GetACL(ctx, account, mailbox)
	if err != nil {
		return err
	}
	if rights == "" {
		delete(cur, identifier)
	} else {
		cur[identifier] = rights
	}
	return k.putACL(ctx, account, mailbox, cur)
}

// DeleteACL removes every right of identifier.
func (k *KV) DeleteACL(ctx context.Context, account, mailbox, identifier string) error {
	return k.SetACL(ctx, account, mailbox, identifier, "")
}

func (k *KV) putACL(ctx context.Context, account, mailbox string, acl map[string]string) error {
	acctID, mbID, err := k.aclDoc(ctx, account, mailbox)
	if err != nil {
		return err
	}
	if len(acl) == 0 {
		return k.s.PutDocumentFields(ctx, acctID, store.CollectionMailbox, mbID, map[byte][]byte{mbFieldACL: nil})
	}
	raw, err := json.Marshal(acl)
	if err != nil {
		return err
	}
	return k.s.PutDocumentFields(ctx, acctID, store.CollectionMailbox, mbID, map[byte][]byte{mbFieldACL: raw})
}

// aclDoc resolves the mailbox document, lazily provisioning INBOX so ACL
// reads on a fresh account behave like LIST (RFC 3501: INBOX always exists).
func (k *KV) aclDoc(ctx context.Context, account, mailbox string) (store.AccountID, uint64, error) {
	acctID, err := k.ensureAccount(ctx, account)
	if err != nil {
		return 0, 0, err
	}
	if mailbox == "INBOX" {
		mbID, err := k.ensureMailbox(ctx, acctID, "INBOX")
		return acctID, mbID, err
	}
	mbID, err := k.mailboxDocID(ctx, acctID, mailbox)
	return acctID, mbID, err
}
