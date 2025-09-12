// Memoised mailbox listings.
package imap

import (
	"context"
	"strconv"

	"mailezine/internal/mailstore"
)

// maxCachedListingMessages bounds what the listing memo stores. A listing is
// ~100KB for 500 messages (UID, sender, flags, size, modseq per entry), so a
// mailbox past this size is listed on every call instead of pinning the cache.
const maxCachedListingMessages = 2000

// mailboxListAt returns the mailbox listing for the given version, reusing a
// memoised one when the mailbox has not changed since it was captured.
//
// The CONDSTORE modseq is the mailbox's version — every visible change
// (delivery, flag change, expunge, move) bumps it, which is the invariant
// Poll's gate already relies on. Keying the memo by it makes the entry
// self-invalidating: a listing can only be reached through the version it was
// captured at, so two nodes that both changed the mailbox cannot serve each
// other's stale copy. The uidvalidity is part of the key as well, because a
// delete+recreate of the same name restarts the modseq (the reused UIDs would
// otherwise hit the old mailbox's listing). That is what lets this run on
// multi-active nodes, where the TTL'd mailbox-metadata cache has to stay off.
//
// modSeq/gated come from the caller's MailboxModSeq read, which must happen
// BEFORE the listing (read-order discipline, see Poll): a listing may then
// include changes newer than the version it is filed under. That costs a
// spurious diff later — never a skipped one — and the next call sees the
// newer modseq and misses anyway.
func (s *session) mailboxListAt(ctx context.Context, mailbox string, uidvalidity uint32, modSeq uint64, gated bool) ([]*mailstore.Message, error) {
	if gated {
		if v, ok := s.srv.cache.Get(listingKey(s.user, mailbox, uidvalidity, modSeq)); ok {
			return cloneListing(v.([]*mailstore.Message)), nil
		}
	}
	msgs, err := s.srv.Store.ListMessages(ctx, s.user, mailbox)
	if err != nil {
		return nil, err
	}
	if gated && len(msgs) <= maxCachedListingMessages {
		s.srv.cache.Put(listingKey(s.user, mailbox, uidvalidity, modSeq), cloneListing(msgs), listingWeight(msgs))
	}
	return msgs, nil
}

func listingKey(account, mailbox string, uidvalidity uint32, modSeq uint64) string {
	return "list\x00" + account + "\x00" + mailbox + "\x00" +
		strconv.FormatUint(uint64(uidvalidity), 10) + "\x00" + strconv.FormatUint(modSeq, 10)
}

// cloneListing hands every caller its own copy: sessions mutate the flags of
// their snapshot entries (a non-PEEK FETCH sets \Seen), so a shared value
// would leak one session's writes into another's view.
func cloneListing(in []*mailstore.Message) []*mailstore.Message {
	if in == nil {
		return nil
	}
	out := make([]*mailstore.Message, len(in))
	for i, m := range in {
		cp := *m
		cp.Flags = append([]string(nil), m.Flags...)
		cp.Keywords = append([]string(nil), m.Keywords...)
		cp.To = append([]string(nil), m.To...)
		out[i] = &cp
	}
	return out
}

// listingWeight mirrors the mailstore's own estimate for the same structs.
func listingWeight(msgs []*mailstore.Message) int64 {
	var w int64
	for _, m := range msgs {
		w += 96 + int64(len(m.Flags))*16 + int64(len(m.Keywords))*24 +
			int64(len(m.From)) + int64(len(m.To))*16
	}
	return w
}
