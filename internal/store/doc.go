package store

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
)

// EnsureMailboxDoc atomically resolves the mailbox document registered under
// name, creating it (doc counter + meta sentinel + fields + name index) when
// absent. The whole check-create sequence runs inside one transaction under
// the account shard lock, so concurrent first deliveries to the same new
// mailbox cannot fork duplicate documents: in-process callers serialize on
// the shard lock, and on optimistic backends a conflicting create replays
// the closure and adopts the winner's index entry (INV-DELIVERY / INV-UID).
func (s *Store) EnsureMailboxDoc(ctx context.Context, accountID AccountID, name string, fields func(docID uint64) map[byte][]byte) (uint64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()

	idxKey := IndexMailboxNameKey(uint32(accountID), name)
	counterKey := CounterKey(uint32(accountID), CounterKindNextDoc, []byte{CollectionMailbox})
	var docID uint64
	err := s.txn.WithTxn(ctx, func(t TxnOps) error {
		if v, err := t.Get(idxKey); err == nil {
			if len(v) != 8 {
				return errors.New("store: corrupt mailbox name index")
			}
			docID = binary.BigEndian.Uint64(v)
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		next, err := readCounter(t.Get, counterKey)
		if err != nil {
			return err
		}
		docID = next + 1
		ops := []Op{
			{Key: counterKey, Value: beUint64(docID)},
			{Key: FieldKey(uint32(accountID), CollectionMailbox, docID, FieldMeta), Value: []byte{1}},
			{Key: idxKey, Value: beUint64(docID)},
		}
		for f, v := range fields(docID) {
			ops = append(ops, Op{Key: FieldKey(uint32(accountID), CollectionMailbox, docID, f), Value: v})
		}
		t.Append(ops...)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return docID, nil
}

// CreateDocument allocates the next document ID in a collection and marks the
// document as existing. IDs are monotonic and never reused (INV-UID).
func (s *Store) CreateDocument(ctx context.Context, accountID AccountID, collection byte) (uint64, error) {
	unlock := s.lockAccount(accountID)
	defer unlock()

	counterKey := CounterKey(uint32(accountID), CounterKindNextDoc, []byte{collection})
	var docID uint64
	err := s.txn.WithTxn(ctx, func(t TxnOps) error {
		next, err := readCounter(t.Get, counterKey)
		if err != nil {
			return err
		}
		docID = next + 1
		t.Append(
			Op{Key: counterKey, Value: beUint64(docID)},
			Op{Key: FieldKey(uint32(accountID), collection, docID, FieldMeta), Value: []byte{1}},
		)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return docID, nil
}

// PutDocumentFields upserts document fields in one atomic batch.
func (s *Store) PutDocumentFields(_ context.Context, accountID AccountID, collection byte, docID uint64, fields map[byte][]byte) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	ops := make([]Op, 0, len(fields))
	for field, value := range fields {
		ops = append(ops, Op{Key: FieldKey(uint32(accountID), collection, docID, field), Value: value})
	}
	return s.kv.Batch(ops)
}

// GetDocumentFields reads every field of a document. A missing document
// (no meta sentinel) returns ErrNotFound.
func (s *Store) GetDocumentFields(_ context.Context, accountID AccountID, collection byte, docID uint64) (map[byte][]byte, error) {
	prefix := DocumentKey(uint32(accountID), collection, docID)
	fields := map[byte][]byte{}
	found := false
	err := s.kv.Scan(prefix, func(k, v []byte) error {
		found = true
		field := k[len(k)-1]
		val := append([]byte(nil), v...)
		fields[field] = val
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return fields, nil
}

// DeleteDocument removes every field of a document in one atomic batch.
func (s *Store) DeleteDocument(ctx context.Context, accountID AccountID, collection byte, docID uint64) error {
	unlock := s.lockAccount(accountID)
	defer unlock()

	prefix := DocumentKey(uint32(accountID), collection, docID)
	var ops []Op
	err := s.kv.Scan(prefix, func(k, _ []byte) error {
		ops = append(ops, Op{Key: append([]byte(nil), k...), Delete: true})
		return nil
	})
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return ErrNotFound
	}
	return s.kv.Batch(ops)
}

// ListDocumentIDs returns the sorted document IDs of a collection.
func (s *Store) ListDocumentIDs(_ context.Context, accountID AccountID, collection byte) ([]uint64, error) {
	seen := map[uint64]struct{}{}
	err := s.kv.Scan(CollectionKey(uint32(accountID), collection), func(k, _ []byte) error {
		if len(k) < 15 {
			return errors.New("store: corrupt field key")
		}
		seen[DocumentIDFromFieldKey(k)] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}
