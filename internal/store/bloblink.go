package store

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"time"
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

// UnlinkBlob decrements the reference counter; at zero the counter is
// tombstoned for the GC path and the blob is claimed for the sweep
// (MetaBlobGCKey). Unlinking an absent or already-reclaimed link is a bug in
// the caller and returns ErrNotFound.
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
// aborted attempt and stick). A first link (counter absent or zero) also
// drops any GC claim for the content ID: the delivery re-created the file
// before this commit, so the sweep must never reclaim it.
func stageBlobLink(t TxnOps, accountID AccountID, blobID string) error {
	key := BlobLinkKey(uint32(accountID), blobID)
	refs, err := blobRefValue(t.Get, key)
	if errors.Is(err, ErrNotFound) {
		refs = 0 // first link
	} else if err != nil {
		return err
	}
	if refs == 0 {
		t.Delete(MetaBlobGCKey(blobID))
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
	if refs == 0 {
		// Zero is the reclaimed tombstone (see below): unlinking it is a
		// caller bug and stays loud, exactly like an absent link.
		return ErrNotFound
	}
	if refs == 1 {
		// Tombstone the counter at zero instead of deleting the row: the
		// zero branch stays reachable and the sweep's liveness scan can
		// distinguish "gone" from "never linked". The blob itself is NOT
		// reclaimed here: content-addressed blob IDs are global while link
		// counts are per-account, so only the globally-verified sweep may
		// delete the file. This transaction stages the GC claim; the sweep
		// (SweepBlobs) reclaims after the grace period once no account links
		// the content anywhere.
		t.Put(key, beUint64(0))
		t.Put(MetaBlobGCKey(blobID), beUint64(uint64(time.Now().Unix())))
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

// BlobStat returns the stored size of a blob (ErrNotFound when absent).
func (s *Store) BlobStat(ctx context.Context, id string) (int64, error) {
	return s.blob.Stat(ctx, id)
}

// ScanBlobLinkSpace visits every blob link row across all accounts:
// refs counts of zero are tombstones left by the delete path and are still
// reported (the sweep treats them as not-live).
func (s *Store) ScanBlobLinkSpace(_ context.Context, fn func(accountID uint32, blobID string, refs uint64) error) error {
	prefix := []byte{SpaceBlobLink}
	return s.kv.Scan(prefix, func(k, v []byte) error {
		if len(k) < 5 || len(v) != 8 {
			return nil // not a well-formed link row; skip
		}
		acct := binary.BigEndian.Uint32(k[1:5])
		return fn(acct, string(k[5:]), binary.BigEndian.Uint64(v))
	})
}

// ScanBlobGCClaims visits every pending blob GC claim with the unix time it
// was queued.
func (s *Store) ScanBlobGCClaims(_ context.Context, fn func(blobID string, queuedUnix int64) error) error {
	return s.kv.Scan(MetaBlobGCPrefix(), func(k, v []byte) error {
		if len(v) != 8 {
			return nil
		}
		return fn(string(k[len(MetaBlobGCPrefix()):]), int64(binary.BigEndian.Uint64(v)))
	})
}

// DeleteBlobGCClaim removes a processed (or stale) GC claim.
func (s *Store) DeleteBlobGCClaim(_ context.Context, blobID string) error {
	return s.kv.Delete(MetaBlobGCKey(blobID))
}

// SweepBlobs runs one mark-and-sweep pass over blob storage. A claimed blob
// is physically reclaimed only when (a) its claim is older than grace —
// deliveries re-linking the same content drop the claim in their commit, so
// the grace window absorbs any delivery still between its file write and its
// link commit — and (b) no account links it anywhere (mark phase over the
// whole link space). Claims for still-live blobs are stale and dropped.
// reclaim does the backend-specific physical delete; returning an error from
// it keeps the claim for the next pass.
func (s *Store) SweepBlobs(ctx context.Context, grace time.Duration, now time.Time, reclaim func(ctx context.Context, blobID string) error) (int, error) {
	live := map[string]struct{}{}
	zeroRows := map[string][]uint32{}
	err := s.ScanBlobLinkSpace(ctx, func(accountID uint32, blobID string, refs uint64) error {
		if refs > 0 {
			live[blobID] = struct{}{}
		} else {
			zeroRows[blobID] = append(zeroRows[blobID], accountID)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	var claims []struct {
		blobID string
		queued int64
	}
	err = s.ScanBlobGCClaims(ctx, func(blobID string, queuedUnix int64) error {
		claims = append(claims, struct {
			blobID string
			queued int64
		}{blobID, queuedUnix})
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].queued < claims[j].queued })

	reclaimed := 0
	for _, c := range claims {
		if ctx.Err() != nil {
			return reclaimed, ctx.Err()
		}
		if _, ok := live[c.blobID]; ok {
			// Re-linked before the sweep got here: stale claim.
			if err := s.DeleteBlobGCClaim(ctx, c.blobID); err != nil {
				return reclaimed, err
			}
			continue
		}
		if now.Sub(time.Unix(c.queued, 0)) < grace {
			continue // still inside the grace window
		}
		if err := reclaim(ctx, c.blobID); err != nil {
			if errors.Is(err, ErrNotFound) {
				// Already gone (previous pass crashed after reclaim):
				// the claim is done either way.
			} else {
				return reclaimed, err
			}
		}
		// Drop the claim and the zero-count link tombstones: the blob is
		// physically gone, so its rows would only accumulate.
		if err := s.DeleteBlobGCClaim(ctx, c.blobID); err != nil {
			return reclaimed, err
		}
		for _, acct := range zeroRows[c.blobID] {
			if err := s.kv.Delete(BlobLinkKey(acct, c.blobID)); err != nil {
				return reclaimed, err
			}
		}
		reclaimed++
	}
	return reclaimed, nil
}
