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
	// Mailbox and UID are carried by extended email-delete entries so
	// consumers that maintain derived state (the FTS tailer) can address
	// the exact copy after the KV row is gone. Empty on legacy entries.
	Mailbox string
	UID     uint32
}

// AppendChange records one change and returns its monotonic change ID
// (INV-CHANGE: the log is append-only, gapless and ordered per account).
func (s *Store) AppendChange(ctx context.Context, accountID AccountID, collection byte, docID uint64, op byte) (uint64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()

	counterKey := CounterKey(uint32(accountID), CounterKindChange, nil)
	var changeID uint64
	err := s.txn.WithTxn(ctx, func(t TxnOps) error {
		next, err := readCounter(t.Get, counterKey)
		if err != nil {
			return err
		}
		changeID = next + 1
		t.Append(
			Op{Key: counterKey, Value: beUint64(changeID)},
			Op{Key: ChangeLogKey(uint32(accountID), collection, changeID), Value: encodeChangeValue(collection, docID, op)},
		)
		return nil
	})
	if err != nil {
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

// encodeEmailDeleteValue extends a delete record with the deleted copy's
// mailbox name and UID. Derived-state consumers (the FTS tailer) cannot
// recover them after the fact — the KV row is gone by the time the change
// is read. Layout: the 10-byte base record, then UID (8-byte BE, the
// EmailFieldUID convention), then a u16 length prefix and the mailbox name.
func encodeEmailDeleteValue(collection byte, docID uint64, mailbox string, uid uint32) []byte {
	value := encodeChangeValue(collection, docID, OpDelete)
	var uidBuf [8]byte
	binary.BigEndian.PutUint64(uidBuf[:], uint64(uid))
	value = append(value, uidBuf[:]...)
	value = binary.BigEndian.AppendUint16(value, uint16(len(mailbox)))
	return append(value, mailbox...)
}

// ChangesSince returns changes with changeID > after, in ascending order.
// The read seeks to the cursor key instead of scanning the whole prefix:
// pollers (the FTS tailer) call this every few seconds, and a prefix scan
// would make each poll O(total change history) per account.
func (s *Store) ChangesSince(_ context.Context, accountID AccountID, collection byte, after uint64) ([]Change, error) {
	prefix := ChangeLogPrefix(uint32(accountID), collection)
	start := ChangeLogKey(uint32(accountID), collection, after+1)
	var out []Change
	err := s.kv.ScanRange(start, PrefixEnd(prefix), func(k, v []byte) error {
		out = append(out, Change{
			ChangeID:   ChangeIDFromLogKey(k),
			Collection: v[0],
			DocID:      binary.BigEndian.Uint64(v[1:9]),
			Op:         v[9],
		})
		// Extended email-delete entries carry the copy's (mailbox, UID).
		ch := &out[len(out)-1]
		if ch.Op == OpDelete && len(v) >= 20 {
			ch.UID = uint32(binary.BigEndian.Uint64(v[10:18]))
			if n := int(binary.BigEndian.Uint16(v[18:20])); n > 0 && len(v) >= 20+n {
				ch.Mailbox = string(v[20 : 20+n])
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
