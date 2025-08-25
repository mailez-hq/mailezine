package store

import (
	"context"
	"encoding/binary"
	"errors"
)

// AddQuotaUsed adjusts the used-bytes counter of an account and returns the
// new value. Higher layers guarantee conservation (INV-QUOTA); the primitive
// keeps exact arithmetic so drift is visible in tests.
func (s *Store) AddQuotaUsed(_ context.Context, accountID AccountID, delta int64) (int64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()

	key := QuotaKey(uint32(accountID))
	cur, err := s.quotaCounter(key)
	if err != nil {
		return 0, err
	}
	nv := cur + delta
	if err := s.kv.Put(key, beUint64(uint64(nv))); err != nil {
		return 0, err
	}
	return nv, nil
}

// QuotaUsed returns the used-bytes counter of an account (0 when unset).
func (s *Store) QuotaUsed(_ context.Context, accountID AccountID) (int64, error) {
	return s.quotaCounter(QuotaKey(uint32(accountID)))
}

func (s *Store) quotaCounter(key []byte) (int64, error) {
	v, err := s.kv.Get(key)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, errors.New("store: corrupt quota counter")
	}
	return int64(binary.BigEndian.Uint64(v)), nil
}
