// Key encoding for the KV backends, aligned with ARCHITECTURE.md §3.2.
//
// Layout: space(1) | accountID(4 BE) | collection(1) | documentID(8 BE) |
// field(1) | value…  All keys are byte-order-sortable, so prefix scans give
// stable logical order regardless of backend.
package store

import (
	"encoding/binary"
)

// Space prefixes (ARCHITECTURE.md §3.2).
const (
	SpaceMeta     byte = 'm'
	SpaceAccount  byte = 'a'
	SpaceIndex    byte = 'i'
	SpaceDocument byte = 'd'
	SpaceLog      byte = 'l'
	SpaceBlobLink byte = 'b'
	SpaceQueue    byte = 'q'
	SpaceQuota    byte = 'u'
)

// Index tag bytes inside the index space (first byte after accountID).
const (
	IdxMailboxName byte = 'n' // (account, mailbox name) → mailbox docID
	IdxEmailMbox   byte = 'e' // (account, mailbox docID, UID) → email docID
	// IdxEmailExpunged is the QRESYNC (RFC 7162) tombstone index:
	// (account, mailbox docID, expunge modseq) → UID. Written in the same
	// atomic batch as the email deletion, so a crash cannot lose the
	// tombstone while dropping the message.
	IdxEmailExpunged byte = 'x'
)

// Counter kinds inside the account space (ARCHITECTURE.md §5.1).
const (
	CounterKindNextDoc byte = 0x01 // per (account, collection)
	CounterKindChange  byte = 0x02 // per account
)

// Collection identifiers for the v1 logical model (ARCHITECTURE.md §2.1).
const (
	CollectionMailbox         byte = 1
	CollectionEmail           byte = 2
	CollectionThread          byte = 3
	CollectionIdentity        byte = 4
	CollectionEmailSubmission byte = 5
	CollectionSieveScript     byte = 6
	CollectionPrincipal       byte = 7
)

// FieldMeta is the sentinel field marking a document as existing. An empty
// document (created but not yet populated) is still discoverable.
const FieldMeta byte = 0x00

// FieldMailboxModSeq is the mailbox document field holding the CONDSTORE
// modification sequence (HIGHESTMODSEQ).
const FieldMailboxModSeq byte = 0x7f

// AccountKey returns the key of the account record.
func AccountKey(accountID uint32) []byte {
	return appendSpaceID(SpaceAccount, accountID)
}

// CollectionKey returns the prefix of every document in a collection.
func CollectionKey(accountID uint32, collection byte) []byte {
	k := appendSpaceID(SpaceDocument, accountID)
	return append(k, collection)
}

// DocumentKey returns the key of a single document.
func DocumentKey(accountID uint32, collection byte, documentID uint64) []byte {
	k := CollectionKey(accountID, collection)
	return appendUint64(k, documentID)
}

// FieldKey returns the key of one field of a document.
func FieldKey(accountID uint32, collection byte, documentID uint64, field byte) []byte {
	k := DocumentKey(accountID, collection, documentID)
	return append(k, field)
}

// QueueKey returns the key of a queued message under SpaceQueue.
func QueueKey(queueName string, messageID uint64) []byte {
	k := []byte{SpaceQueue}
	k = append(k, queueName...)
	k = append(k, 0)
	return appendUint64(k, messageID)
}

// QueueMetaPrefix is the scan prefix for all queue metadata entries.
func QueueMetaPrefix(queueName string) []byte {
	k := []byte{SpaceQueue}
	k = append(k, queueName...)
	return append(k, 0)
}

// QueueCounterKey is the global message-ID allocator of the outbound queue.
func QueueCounterKey() []byte {
	k := []byte{SpaceQueue}
	return append(k, "counter"...)
}

// QueueDueKey indexes a queued message by its next-attempt time (8-byte BE
// unix seconds) then message ID, so a prefix scan yields due messages in
// time order.
func QueueDueKey(nextAttemptUnix int64, messageID uint64) []byte {
	k := []byte{SpaceQueue}
	k = append(k, "due/"...)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(nextAttemptUnix))
	k = append(k, buf[:]...)
	return appendUint64(k, messageID)
}

// QueueDuePrefix is the scan prefix of the due index.
func QueueDuePrefix() []byte {
	return append([]byte{SpaceQueue}, "due/"...)
}

// CounterKey addresses a per-account counter (next doc ID, change ID, …).
func CounterKey(accountID uint32, kind byte, sub []byte) []byte {
	k := appendSpaceID(SpaceAccount, accountID)
	k = append(k, kind)
	return append(k, sub...)
}

// ChangeLogKey addresses one change-log entry.
func ChangeLogKey(accountID uint32, collection byte, changeID uint64) []byte {
	k := []byte{SpaceLog}
	k = appendUint32(k, accountID)
	k = append(k, collection)
	return appendUint64(k, changeID)
}

// ChangeLogPrefix is the scan prefix of one account+collection change log.
func ChangeLogPrefix(accountID uint32, collection byte) []byte {
	k := []byte{SpaceLog}
	k = appendUint32(k, accountID)
	return append(k, collection)
}

// QuotaKey addresses the used-bytes counter of an account.
func QuotaKey(accountID uint32) []byte {
	return appendSpaceID(SpaceQuota, accountID)
}

// BlobLinkKey addresses the reference counter of one blob in one account.
func BlobLinkKey(accountID uint32, blobID string) []byte {
	k := appendSpaceID(SpaceBlobLink, accountID)
	return append(k, blobID...)
}

// MetaEmailKey maps an email to its account ID (global registry).
func MetaEmailKey(email string) []byte {
	k := []byte{SpaceMeta}
	k = append(k, "email:"...)
	return append(k, email...)
}

// IndexMailboxNameKey maps (account, mailbox name) to the mailbox document
// ID. Maintained in the same batch as the mailbox document when practical;
// a stale entry self-heals on resolution (mailstore falls back to a linear
// scan and repairs).
func IndexMailboxNameKey(accountID uint32, name string) []byte {
	k := appendSpaceID(SpaceIndex, accountID)
	k = append(k, IdxMailboxName)
	return append(k, name...)
}

// IndexEmailKey maps (account, mailbox docID, UID) to the email document ID.
// Ascending key order yields per-mailbox messages in UID order.
func IndexEmailKey(accountID uint32, mbID, uid uint64) []byte {
	k := appendSpaceID(SpaceIndex, accountID)
	k = append(k, IdxEmailMbox)
	k = appendUint64(k, mbID)
	return appendUint64(k, uid)
}

// IndexEmailPrefix is the scan prefix of one mailbox's email index.
func IndexEmailPrefix(accountID uint32, mbID uint64) []byte {
	k := appendSpaceID(SpaceIndex, accountID)
	k = append(k, IdxEmailMbox)
	return appendUint64(k, mbID)
}

// IndexExpungeKey maps one expunged message to its expunge modseq:
// (account, mailbox docID, modseq, UID). The UID is part of the key because
// a batch expunge of N messages shares one expunge modseq — without the UID
// component the N tombstones would collapse onto one key, each write
// overwriting the last, and VANISHED (EARLIER) would report only the final
// UID of every batch (silent client desync). Ascending key order still
// yields tombstones in (modseq, UID) order, so a range scan answers
// "vanished since modseq M" in one pass.
func IndexExpungeKey(accountID uint32, mbID, modSeq, uid uint64) []byte {
	k := IndexExpungePrefix(accountID, mbID)
	k = appendUint64(k, modSeq)
	return appendUint64(k, uid)
}

// IndexExpungePrefix is the scan prefix of one mailbox's expunge log.
func IndexExpungePrefix(accountID uint32, mbID uint64) []byte {
	k := appendSpaceID(SpaceIndex, accountID)
	k = append(k, IdxEmailExpunged)
	return appendUint64(k, mbID)
}

// MetaNextAccountKey is the global account-ID allocator.
func MetaNextAccountKey() []byte {
	k := []byte{SpaceMeta}
	return append(k, "next_account"...)
}

// MetaLeaseKey addresses a named coordination lease in the meta space —
// singleton worker election in multi-active deployments. The value is the
// JSON record {owner, until}; fencing is the read-modify-write transaction
// of the underlying TxnKV.
func MetaLeaseKey(name string) []byte {
	k := []byte{SpaceMeta}
	k = append(k, "lease:"...)
	return append(k, name...)
}

// MetaVacationKey returns the key of the vacation last-sent state for
// (account, sender).
func MetaVacationKey(account, sender string) []byte {
	k := []byte{SpaceMeta}
	k = append(k, "vacation:"...)
	k = append(k, account...)
	k = append(k, '\x1f')
	return append(k, sender...)
}

// MetaBlobGCKey claims a blob as physically reclaimable: written in the
// same transaction that drops an account's last link (value: BE8 unix
// seconds queued-at), deleted by the sweep after the blob is reclaimed — or
// by a later delivery re-linking the same content-addressed ID. Blobs are
// global (one file per content hash) while link counts are per-account, so
// physical reclamation must be a separate globally-verified step, never an
// inline consequence of one account's refcount hitting zero.
func MetaBlobGCKey(blobID string) []byte {
	return append(MetaBlobGCPrefix(), blobID...)
}

// MetaBlobGCPrefix is the scan prefix of all blob GC claims.
func MetaBlobGCPrefix() []byte {
	k := []byte{SpaceMeta}
	return append(k, "gc:blob:"...)
}

// DocumentIDFromFieldKey extracts the document ID from a field key.
// Layout: space(0) account(1-4) collection(5) documentID(6-13) field(14).
func DocumentIDFromFieldKey(k []byte) uint64 {
	return binary.BigEndian.Uint64(k[6:14])
}

// ChangeIDFromLogKey extracts the change ID from a change-log key.
// Layout: space(0) account(1-4) collection(5) changeID(6-13).
func ChangeIDFromLogKey(k []byte) uint64 {
	return binary.BigEndian.Uint64(k[6:14])
}

func appendSpaceID(space byte, accountID uint32) []byte {
	k := []byte{space}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], accountID)
	return append(k, buf[:]...)
}

func appendUint32(k []byte, v uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(k, buf[:]...)
}

func appendUint64(k []byte, v uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	return append(k, buf[:]...)
}
