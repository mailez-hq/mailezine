package store

import (
	"context"
	"errors"
)

// LinkBlob increments the reference counter of a blob for an account
// (INV-BLOB: every Email document reference has a link). The read-modify-
// write runs inside one transaction under the account lock, so concurrent
// links cannot lose an increment.
func (s *Store) LinkBlob(ctx context.Context, accountID AccountID, blobID string) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	return s.txn.WithTxn(ctx, func(t TxnOps) error {
		return stageBlobLink(t, accountID, blobID)
	})
}

// UnlinkBlob decrements the reference counter; at zero the link is removed.
// Unlinking an absent link is a bug in the caller and returns ErrNotFound.
func (s *Store) UnlinkBlob(ctx context.Context, accountID AccountID, blobID string) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	return s.txn.WithTxn(ctx, func(t TxnOps) error {
		return stageBlobUnlink(t, accountID, blobID)
	})
}

// stageBlobLink stages the refcount increment inside an open transaction.
// Staged writes roll back on closure error, so a replayed optimistic
// transaction cannot double-increment (an eager kv.Put would survive the
// aborted attempt and stick).
func stageBlobLink(t TxnOps, accountID AccountID, blobID string) error {
	key := BlobLinkKey(uint32(accountID), blobID)
	refs, err := blobRefValue(t.Get, key)
	if errors.Is(err, ErrNotFound) {
		refs = 0 // first link
	} else if err != nil {
		return err
	}
	t.Put(key, beUint64(uint64(refs+1)))
	return nil
}

// stageBlobUnlink stages the refcount decrement inside an open transaction.
func stageBlobUnlink(t TxnOps, accountID AccountID, blobID string) error {
	key := BlobLinkKey(uint32(accountID), blobID)
	refs, err := blobRefValue(t.Get, key)
	if err != nil {
		return err
	}
	if refs == 1 {
		t.Delete(key)
		return nil
	}
	t.Put(key, beUint64(uint64(refs-1)))
	return nil
}

// BlobRefCount returns the reference count of a blob in an account.
func (s *Store) BlobRefCount(_ context.Context, accountID AccountID, blobID string) (int64, error) {
	return s.blobRefs(BlobLinkKey(uint32(accountID), blobID))
}

func (s *Store) blobRefs(key []byte) (int64, error) {
	return blobRefValue(s.kv.Get, key)
}
