// QRESYNC (RFC 7162): session-side resynchronization payloads. The conn
// parses the wire commands (SessionQRESYNC optional interface); these
// methods compute VANISHED and flag-update data from the mailstore
// expunge tombstone log and per-message modseqs.
package imap

import (
	"context"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/imapserver"
)

var _ imapserver.SessionQRESYNC = (*session)(nil)

// toUIDs converts store-level UIDs to wire UIDs.
func toUIDs(uids []uint32) []imap.UID {
	if len(uids) == 0 {
		return nil
	}
	out := make([]imap.UID, len(uids))
	for i, u := range uids {
		out[i] = imap.UID(u)
	}
	return out
}

// SelectQRESYNC selects the mailbox and produces the resynchronization
// payload (RFC 7162 §3.2.5.2): VANISHED (EARLIER) UIDs from the expunge
// tombstone log and FLAGS updates for messages changed after the client's
// modseq. A UIDVALIDITY mismatch (mailbox deleted and recreated while the
// client was away) yields empty data — the client's cache is void anyway.
func (s *session) SelectQRESYNC(mailbox string, options *imap.SelectOptions, param imapserver.QRESYNCParam, w *imapserver.UpdateWriter) (*imap.SelectData, *imapserver.QResyncData, error) {
	data, err := s.Select(mailbox, options)
	if err != nil {
		return nil, nil, err
	}
	qd := &imapserver.QResyncData{}
	if param.UIDValidity != data.UIDValidity {
		return data, qd, nil
	}
	ctx := context.Background()
	vanished, err := s.srv.Store.ExpungedSince(ctx, s.user, mailbox, param.ModSeq)
	if err != nil {
		return nil, nil, err
	}
	qd.VanishedEarlier = toUIDs(vanished)
	msgs, err := s.snapshotMsgs(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i, m := range msgs {
		// Strictly after the client's modseq: anything at or below it is
		// already reflected in the client's cache.
		if m.ModSeq <= param.ModSeq {
			continue
		}
		qd.FlagUpdates = append(qd.FlagUpdates, imapserver.QResyncFlagUpdate{
			SeqNum: uint32(i) + 1,
			UID:    imap.UID(m.UID),
			Flags:  imapFlags(m.Flags),
			ModSeq: m.ModSeq,
		})
	}
	return data, qd, nil
}

// FetchQRESYNC serves UID FETCH ... (CHANGEDSINCE modseq [VANISHED]):
// VANISHED (from the tombstone log) first, then the flags fetch restricted
// to messages whose modseq is strictly after the client's modseq.
func (s *session) FetchQRESYNC(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions, sinceModSeq uint64, vanished bool) error {
	ctx := context.Background()
	if vanished {
		uids, err := s.srv.Store.ExpungedSince(ctx, s.user, s.mbox, sinceModSeq)
		if err != nil {
			return err
		}
		if err := w.WriteVanished(toUIDs(uids)); err != nil {
			return err
		}
	}
	msgs, err := s.snapshotMsgs(ctx)
	if err != nil {
		return err
	}
	maxSeq := uint32(len(msgs))
	maxUID := maxSeq
	if maxSeq > 0 {
		maxUID = msgs[maxSeq-1].UID
	}
	var set imap.UIDSet
	for i, m := range msgs {
		if m.ModSeq <= sinceModSeq {
			continue
		}
		if !numMatches(numSet, uint32(i)+1, m.UID, maxSeq, maxUID) {
			continue
		}
		set.AddNum(imap.UID(m.UID))
	}
	// RFC 7162 §3.1.4.1: CHANGEDSINCE implies MODSEQ in the response.
	if !options.ModSeq {
		options.ModSeq = true
	}
	// numContains matches on the concrete value types (SeqSet/UIDSet), so
	// pass the set by value — a *UIDSet would fall through every case.
	return s.Fetch(w, set, options)
}
