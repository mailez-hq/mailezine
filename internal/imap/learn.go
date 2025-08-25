// Junk-boundary learning: when a message enters or leaves the Junk mailbox
// through APPEND/COPY/MOVE, hand its bytes to the configured learner
// (rspamd learn_spam/learn_ham). Learning is best-effort.
package imap

import (
	"context"
	"io"
	"strings"
)

// isJunkMailbox reports whether a mailbox is the Junk mailbox
// (case-insensitive).
func isJunkMailbox(name string) bool {
	return strings.EqualFold(name, "Junk")
}

// learnMessage fetches one message and hands it to the learner. Errors are
// logged by the caller via learnErr; the IMAP operation itself never fails
// on learning problems.
func (s *session) learnMessage(ctx context.Context, mailbox string, uid uint32, isSpam bool) {
	if s.srv.Learn == nil {
		return
	}
	rc, err := s.srv.Store.OpenMessage(ctx, s.user, mailbox, uid)
	if err != nil {
		s.srv.Logger.Error("imap: learn open", "mailbox", mailbox, "uid", uid, "err", err)
		return
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		s.srv.Logger.Error("imap: learn read", "mailbox", mailbox, "uid", uid, "err", err)
		return
	}
	s.srv.Learn(ctx, s.user, isSpam, data)
}

// learnCopyMove applies the Junk learning rule to a COPY/MOVE:
// into Junk trains spam, out of Junk trains ham. The destination copy is
// read (for MOVE the source is gone after the operation).
func (s *session) learnCopyMove(ctx context.Context, srcMailbox, dstMailbox string, srcUID, dstUID uint32) {
	srcJunk := isJunkMailbox(srcMailbox)
	dstJunk := isJunkMailbox(dstMailbox)
	switch {
	case !srcJunk && dstJunk:
		s.learnMessage(ctx, dstMailbox, dstUID, true)
	case srcJunk && !dstJunk:
		s.learnMessage(ctx, dstMailbox, dstUID, false)
	}
}

// indexCopyMove keeps the FTS index consistent with COPY/MOVE: the
// destination copy is indexed with its new UID; a MOVE also removes the
// source document (COPY keeps it).
func (s *session) indexCopyMove(ctx context.Context, srcMailbox, dstMailbox string, srcUID, dstUID uint32, isMove bool) {
	ix := s.srv.FTS
	if ix == nil {
		return
	}
	if isMove && srcMailbox != dstMailbox {
		_ = ix.DeleteMessage(ctx, s.user, srcMailbox, srcUID)
	}
	rc, err := s.srv.Store.OpenMessage(ctx, s.user, dstMailbox, dstUID)
	if err != nil {
		s.srv.Logger.Error("imap: fts open for index", "mailbox", dstMailbox, "uid", dstUID, "err", err)
		return
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		s.srv.Logger.Error("imap: fts read for index", "mailbox", dstMailbox, "uid", dstUID, "err", err)
		return
	}
	if err := ix.IndexMessage(ctx, s.user, dstMailbox, dstUID, data); err != nil {
		s.srv.Logger.Error("imap: fts index", "mailbox", dstMailbox, "uid", dstUID, "err", err)
	}
}
