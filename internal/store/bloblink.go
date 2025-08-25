package store

import (
	"context"
	"encoding/binary"
	"errors"
)

// LinkBlob increments the reference counter of a blob for an account
// (INV-BLOB: every Email document reference has a link).
func (s *Store) LinkBlob(_ context.Context, accountID AccountID, blobID string) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	key := BlobLinkKey(uint32(accountID), blobID)
	n, err := s.blobRefs(key)
	if errors.Is(err, ErrNotFound) {
		n = 0 // first link
	} else if err != nil {
		return err
	}
	return s.kv.Put(key, beUint64(uint64(n+1)))
}

// UnlinkBlob decrements the reference counter; at zero the link is removed.
// Unlinking an absent link is a bug in the caller and returns ErrNotFound.
func (s *Store) UnlinkBlob(_ context.Context, accountID AccountID, blobID string) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	key := BlobLinkKey(uint32(accountID), blobID)
	n, err := s.blobRefs(key)
	if err != nil {
		return err
	}
	if n == 1 {
		return s.kv.Delete(key)
	}
	return s.kv.Put(key, beUint64(uint64(n-1)))
}

// BlobRefCount returns the reference count of a blob in an account.
func (s *Store) BlobRefCount(_ context.Context, accountID AccountID, blobID string) (int64, error) {
	return s.blobRefs(BlobLinkKey(uint32(accountID), blobID))
}

func (s *Store) blobRefs(key []byte) (int64, error) {
	v, err := s.kv.Get(key)
	if errors.Is(err, ErrNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, errors.New("store: corrupt blob link")
	}
	return int64(binary.BigEndian.Uint64(v)), nil
}
