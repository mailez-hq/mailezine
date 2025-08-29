// STORE implementation: flag add/remove/replace with optional non-silent
// FETCH responses.
package imap

import (
	"context"
	"strings"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/imapserver"

	"mailezine/internal/mailstore"
)

func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	ctx := context.Background()
	msgs, err := s.srv.Store.ListMessages(ctx, s.user, s.mbox)
	if err != nil {
		return err
	}
	// RFC 7162 §3.3: messages skipped due to UNCHANGEDSINCE must be reported
	// in the tagged OK via [MODIFIED <set>] so the client can re-fetch and
	// retry; silently dropping them leaves the client believing its STORE
	// applied. The set uses UIDs for UID STORE, sequence numbers otherwise.
	_, uidStore := numSet.(*imap.UIDSet)
	var modifiedUIDs imap.UIDSet
	var modifiedSeqs imap.SeqSet
	skipped := 0
	maxSeq := uint32(len(msgs))
	maxUID := maxSeq
	if maxSeq > 0 {
		maxUID = msgs[maxSeq-1].UID
	}
	for i, msg := range msgs {
		seq := uint32(i) + 1
		if !numMatches(numSet, seq, msg.UID, maxSeq, maxUID) {
			continue
		}
		// RFC 7162: UNCHANGEDSINCE — skip messages modified after the
		// given modseq (the client re-fetches and retries). The special
		// value 0 always succeeds.
		if options.UnchangedSince != 0 && msg.ModSeq > options.UnchangedSince {
			if uidStore {
				modifiedUIDs.AddNum(imap.UID(msg.UID))
			} else {
				modifiedSeqs.AddNum(seq)
			}
			skipped++
			continue
		}
		next := applyStoreOp(msg.Flags, flags)
		if err := s.srv.Store.SetFlags(ctx, s.user, s.mbox, msg.UID, next); err != nil {
			return err
		}
		if !flags.Silent {
			rw := w.CreateMessage(seq)
			rw.WriteUID(imap.UID(msg.UID))
			rw.WriteFlags(imapFlags(next))
			if err := rw.Close(); err != nil {
				return err
			}
		}
	}
	if err := s.refreshSnapshot(ctx); err != nil {
		return err
	}
	if skipped > 0 {
		if uidStore {
			return &imapserver.StatusOKCode{Code: imapserver.ResponseCodeModified, Set: &modifiedUIDs}
		}
		return &imapserver.StatusOKCode{Code: imapserver.ResponseCodeModified, Set: &modifiedSeqs}
	}
	return nil
}

func applyStoreOp(current []string, store *imap.StoreFlags) []string {
	switch store.Op {
	case imap.StoreFlagsSet:
		out := make([]string, 0, len(store.Flags))
		for _, f := range store.Flags {
			out = append(out, string(f))
		}
		return out
	case imap.StoreFlagsAdd:
		out := append([]string(nil), current...)
		for _, f := range store.Flags {
			if !mailstore.HasFlag(out, string(f)) {
				out = append(out, string(f))
			}
		}
		return out
	case imap.StoreFlagsDel:
		var out []string
		for _, f := range current {
			if !hasStoreFlag(store.Flags, f) {
				out = append(out, f)
			}
		}
		return out
	}
	return current
}

func hasStoreFlag(flags []imap.Flag, want string) bool {
	for _, f := range flags {
		// RFC 3501 §9: keywords are matched case-insensitively. go-imap
		// clients canonicalize keywords to lowercase on fetch, so comparing
		// case-sensitively here would silently fail to remove flags stored
		// with a different case (e.g. $Snoozed vs $snoozed).
		if strings.EqualFold(string(f), want) {
			return true
		}
	}
	return false
}
