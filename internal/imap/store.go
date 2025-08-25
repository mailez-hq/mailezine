// STORE implementation: flag add/remove/replace with optional non-silent
// FETCH responses.
package imap

import (
	"context"

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
	for i, msg := range msgs {
		seq := uint32(i) + 1
		if !numContains(numSet, seq, msg.UID) {
			continue
		}
		// RFC 7162: UNCHANGEDSINCE — skip messages modified after the
		// given modseq (the client re-fetches and retries).
		if options.UnchangedSince != 0 && msg.ModSeq > options.UnchangedSince {
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
	return s.refreshSnapshot(ctx)
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
		if string(f) == want {
			return true
		}
	}
	return false
}
