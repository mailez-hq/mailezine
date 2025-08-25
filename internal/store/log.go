package store

import (
	"context"
	"encoding/binary"
)

// Change-op codes. Higher layers may define their own op semantics; these
// cover the document lifecycle.
const (
	OpCreate byte = 1
	OpUpdate byte = 2
	OpDelete byte = 3
)

// Change is one change-log entry.
type Change struct {
	ChangeID   uint64
	Collection byte
	DocID      uint64
	Op         byte
}

// AppendChange records one change and returns its monotonic change ID
// (INV-CHANGE: the log is append-only, gapless and ordered per account).
func (s *Store) AppendChange(_ context.Context, accountID AccountID, collection byte, docID uint64, op byte) (uint64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()

	counterKey := CounterKey(uint32(accountID), CounterKindChange, nil)
	next, err := s.getCounter(counterKey)
	if err != nil {
		return 0, err
	}
	changeID := next + 1
	value := encodeChangeValue(collection, docID, op)
	ops := []Op{
		{Key: counterKey, Value: beUint64(changeID)},
		{Key: ChangeLogKey(uint32(accountID), collection, changeID), Value: value},
	}
	if err := s.kv.Batch(ops); err != nil {
		return 0, err
	}
	return changeID, nil
}

func encodeChangeValue(collection byte, docID uint64, op byte) []byte {
	value := make([]byte, 10)
	value[0] = collection
	binary.BigEndian.PutUint64(value[1:9], docID)
	value[9] = op
	return value
}

// ChangesSince returns changes with changeID > after, in ascending order.
func (s *Store) ChangesSince(_ context.Context, accountID AccountID, collection byte, after uint64) ([]Change, error) {
	var out []Change
	err := s.kv.Scan(ChangeLogPrefix(uint32(accountID), collection), func(k, v []byte) error {
		changeID := ChangeIDFromLogKey(k)
		if changeID <= after || len(v) != 10 {
			return nil
		}
		out = append(out, Change{
			ChangeID:   changeID,
			Collection: v[0],
			DocID:      binary.BigEndian.Uint64(v[1:9]),
			Op:         v[9],
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
