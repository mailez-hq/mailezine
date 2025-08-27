package store

import (
	"context"
)

// AddQuotaUsed adjusts the used-bytes counter of an account and returns the
// new value. Higher layers guarantee conservation (INV-QUOTA); the primitive
// keeps exact arithmetic so drift is visible in tests.
func (s *Store) AddQuotaUsed(ctx context.Context, accountID AccountID, delta int64) (int64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()

	key := QuotaKey(uint32(accountID))
	var nv int64
	err := s.txn.WithTxn(ctx, func(t TxnOps) error {
		cur, err := quotaValue(t.Get, key)
		if err != nil {
			return err
		}
		nv = cur + delta
		t.Put(key, beUint64(uint64(nv)))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return nv, nil
}

// QuotaUsed returns the used-bytes counter of an account (0 when unset).
func (s *Store) QuotaUsed(_ context.Context, accountID AccountID) (int64, error) {
	return s.quotaCounter(QuotaKey(uint32(accountID)))
}

func (s *Store) quotaCounter(key []byte) (int64, error) {
	return quotaValue(s.kv.Get, key)
}
