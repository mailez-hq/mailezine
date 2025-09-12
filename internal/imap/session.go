// Session implementation: login, mailbox management, message operations.
package imap

import (
	"context"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/imapserver"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
)

// session is one IMAP connection. The mailbox field is the selected
// mailbox name ("" when none).
type session struct {
	srv  *Server
	user string
	mbox string
	// readOnly records EXAMINE (vs SELECT): FETCH must not implicitly set
	// \Seen and the server must not report permanent flags for such a
	// selection (RFC 3501 §6.3.2/§6.4.8).
	readOnly bool
	// uidvalidity of the selected mailbox: fetched-envelope cache keys are
	// namespaced by it, so a delete+recreate (new uidvalidity, same name)
	// cannot serve stale cached envelopes for reused UIDs.
	uidvalidity uint32
	snap        []*mailstore.Message // selected mailbox snapshot for IDLE/POLL diffs
	// snapModSeq and modseqGated back Poll's cheap change check: the
	// mailbox CONDSTORE modseq captured alongside the snapshot, and
	// whether the store maintains one for this mailbox at all.
	snapModSeq  uint64
	modseqGated bool
}

var _ imapserver.Session = (*session)(nil)
var _ imapserver.SessionMove = (*session)(nil)
var _ imapserver.SessionNamespace = (*session)(nil)
var _ imapserver.SessionAppendLimit = (*session)(nil)
var _ imapserver.SessionExtension = (*session)(nil)
var _ imapserver.SessionSort = (*session)(nil)
var _ imapserver.SessionSortUID = (*session)(nil)

// mailboxMetaReader is the optional metadata-only read behind Select. A store
// that has one skips the mailbox-wide message walk MailboxStatus does to fill
// its counters (sizes, unseen, deleted) — SELECT only needs identity fields,
// and it lists the messages anyway to build its snapshot.
type mailboxMetaReader interface {
	MailboxMeta(ctx context.Context, account, mailbox string) (mailstore.Mailbox, error)
}

// mailboxMeta prefers the metadata-only read, falling back to MailboxStatus
// (and its counters) for stores that do not offer one.
func (s *session) mailboxMeta(ctx context.Context, mailbox string) (mailstore.Mailbox, error) {
	if mr, ok := s.srv.Store.(mailboxMetaReader); ok {
		return mr.MailboxMeta(ctx, s.user, mailbox)
	}
	return s.srv.Store.MailboxStatus(ctx, s.user, mailbox)
}

func (s *session) Close() error {
	// Only authenticated sessions were counted on login.
	if s.user != "" {
		s.srv.Metrics.IMAPSessionClosed()
	}
	return nil
}

func (s *session) Login(username, password string) error {
	ok, err := s.srv.Auth.Authenticate(context.Background(), username, password, auth.Options{
		Protocol: "imap",
		Port:     s.srv.Port,
	})
	if err != nil || !ok {
		return imapserver.ErrAuthFailed
	}
	// The directory is the account authority: disabled or unknown accounts
	// cannot sign in even when credentials match.
	u, err := s.srv.Directory.User(context.Background(), username)
	if err != nil || !u.Enabled {
		return imapserver.ErrAuthFailed
	}
	s.user = username
	s.srv.Metrics.IMAPSessionOpened()
	return nil
}

func (s *session) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if mailbox == "" {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "empty mailbox name"}
	}
	ctx := context.Background()
	st, err := s.mailboxMeta(ctx, mailbox)
	if errors.Is(err, mailstore.ErrNotFound) || errors.Is(err, directory.ErrNotFound) {
		// Lazily provision on first use: a fresh account has no mailbox
		// documents until something lists them, and INBOX must always exist
		// (RFC 3501). Listing is that provisioning path — but it also counts
		// every message of the account to fill the LIST counters, and Select
		// discards the result. Paying for it only on the miss keeps the
		// provisioning behaviour while dropping one full email scan from
		// every successful SELECT (~220ms of ~500ms on a 483-message mailbox
		// with the multi-active metadata cache off, measured 2026-09-12).
		if _, lerr := s.srv.Store.ListMailboxes(ctx, s.user); lerr != nil {
			return nil, lerr
		}
		st, err = s.mailboxMeta(ctx, mailbox)
	}
	if err != nil {
		if errors.Is(err, mailstore.ErrNotFound) || errors.Is(err, directory.ErrNotFound) {
			return nil, &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeNonExistent,
				Text: "No such mailbox",
			}
		}
		return nil, err
	}
	// Capture the version before the listing (read-order discipline, see
	// Poll): the listing may then include changes newer than the captured
	// value, which costs a spurious diff later — never a skipped one.
	modSeq, gated := s.srv.Store.MailboxModSeq(ctx, s.user, mailbox)
	msgs, err := s.mailboxListAt(ctx, mailbox, st.UIDValidity, modSeq, gated)
	if err != nil {
		return nil, err
	}
	s.mbox = mailbox
	s.snap = msgs
	s.snapModSeq, s.modseqGated = modSeq, gated
	s.readOnly = options != nil && options.ReadOnly
	s.uidvalidity = st.UIDValidity
	flags := []imap.Flag{
		imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted,
		imap.FlagSeen, imap.FlagDraft,
	}
	permanent := append(append([]imap.Flag(nil), flags...), imap.FlagWildcard)
	if s.readOnly {
		// No permanent flags for an examined mailbox: nothing the client
		// changes will persist.
		permanent = nil
	}
	data := &imap.SelectData{
		Flags:          flags,
		PermanentFlags: permanent,
		// The counters come from the listing above, not from the status: the
		// two must agree, and the listing is what this session will use.
		NumMessages:   uint32(len(msgs)),
		UIDNext:       imap.UID(st.UIDNext),
		UIDValidity:   st.UIDValidity,
		HighestModSeq: st.HighestModSeq,
	}
	for i, msg := range msgs {
		if !mailstore.HasFlag(msg.Flags, "\\Seen") {
			data.FirstUnseenSeqNum = uint32(i) + 1
			break
		}
	}
	return data, nil
}

func (s *session) Unselect() error {
	s.mbox = ""
	return nil
}

func (s *session) Create(mailbox string, options *imap.CreateOptions) error {
	_, err := s.srv.Store.CreateMailbox(context.Background(), s.user, mailbox)
	if errors.Is(err, mailstore.ErrExists) {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeAlreadyExists,
			Text: "Mailbox already exists",
		}
	}
	return err
}

func (s *session) Delete(mailbox string) error {
	if mailbox == "INBOX" {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "INBOX cannot be deleted"}
	}
	err := s.srv.Store.DeleteMailbox(context.Background(), s.user, mailbox)
	if errors.Is(err, mailstore.ErrNotFound) {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
	}
	if err == nil && sameMailbox(s.mbox, mailbox) {
		// RFC 3501 §6.3.4: deleting the currently selected mailbox leaves
		// this session unselected. The pooled-connection clients of the
		// mailez control plane reuse authenticated sessions across API
		// calls; a stale selection would make the next command's poll
		// list the vanished mailbox and kill the connection.
		s.Unselect()
	}
	return err
}

// sameMailbox compares mailbox names with INBOX's case-insensitivity
// (RFC 3501 §5.1); other names compare exactly.
func sameMailbox(a, b string) bool {
	if strings.EqualFold(a, "INBOX") && strings.EqualFold(b, "INBOX") {
		return true
	}
	return a == b
}

func (s *session) Rename(mailbox, newName string, options *imap.RenameOptions) error {
	if mailbox == "INBOX" {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "INBOX cannot be renamed"}
	}
	err := s.srv.Store.RenameMailbox(context.Background(), s.user, mailbox, newName)
	if errors.Is(err, mailstore.ErrNotFound) {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
	}
	if err == nil && s.mbox == mailbox {
		// The selected mailbox survived under its new name; keep the
		// selection attached to it so the session's poll snapshot stays
		// valid instead of listing the vanished old name.
		s.mbox = newName
	}
	return err
}

func (s *session) Subscribe(mailbox string) error {
	return s.srv.Store.SetSubscribed(context.Background(), s.user, mailbox, true)
}

func (s *session) Unsubscribe(mailbox string) error {
	return s.srv.Store.SetSubscribed(context.Background(), s.user, mailbox, false)
}

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	boxes, err := s.srv.Store.ListMailboxes(context.Background(), s.user)
	if err != nil {
		return err
	}
	if len(patterns) == 0 {
		return w.WriteList(&imap.ListData{
			Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect},
			Delim: mailboxDelim,
		})
	}
	for _, mb := range boxes {
		if options.SelectSubscribed && !mb.Subscribed {
			continue
		}
		matched := false
		for _, pattern := range patterns {
			if imapserver.MatchList(mb.Name, mailboxDelim, ref, pattern) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		data := imap.ListData{Mailbox: mb.Name, Delim: mailboxDelim}
		if mb.Subscribed {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		if options.ReturnStatus != nil {
			data.Status = statusData(&mb, options.ReturnStatus)
		}
		if err := w.WriteList(&data); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	mb, err := s.srv.Store.MailboxStatus(context.Background(), s.user, mailbox)
	if err != nil {
		if errors.Is(err, mailstore.ErrNotFound) {
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
		}
		return nil, err
	}
	return statusData(&mb, options), nil
}

func (s *session) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	// Enforce the APPEND limit like SMTP DATA does.
	data, err := io.ReadAll(io.LimitReader(r, s.srv.MaxMessageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.srv.MaxMessageBytes {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "message exceeds size limit"}
	}
	msg := &mailstore.Message{Data: data, InternalDate: options.Time}
	if options.Time.IsZero() {
		msg.InternalDate = time.Now()
	}
	for _, f := range options.Flags {
		msg.Flags = append(msg.Flags, string(f))
	}
	uid, err := s.srv.Store.Append(context.Background(), s.user, mailbox, msg)
	if err != nil {
		if errors.Is(err, mailstore.ErrNotFound) {
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
		}
		return nil, err
	}
	if isJunkMailbox(mailbox) {
		// APPEND into Junk trains spam.
		s.learnMessage(context.Background(), mailbox, uid, true)
	}
	if s.srv.FTS != nil {
		if err := s.srv.FTS.IndexMessage(context.Background(), s.user, mailbox, uid, data); err != nil {
			s.srv.Logger.Error("imap: fts index", "mailbox", mailbox, "uid", uid, "err", err)
		}
	}
	st, err := s.srv.Store.MailboxStatus(context.Background(), s.user, mailbox)
	if err != nil {
		return nil, err
	}
	return &imap.AppendData{UIDValidity: st.UIDValidity, UID: imap.UID(uid)}, nil
}

func (s *session) AppendLimit() uint32 {
	return uint32(s.srv.MaxMessageBytes)
}

func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.mbox == "" {
		return nil
	}
	// Version check first. The CONDSTORE modseq is the version of
	// everything the diff below rebuilds — deliveries, flag changes and
	// expunges all bump it — so "unchanged since the snapshot was
	// captured" skips the full listing, the two maps and the O(mailbox)
	// compare that every command's poll would otherwise pay. Backends
	// without a modseq, and vanished mailboxes (reported unsupported),
	// fall through to the listing path, which owns the error handling.
	//
	// Read-order discipline: the value captured here is committed with
	// the snapshot taken from the listing below. Capturing it BEFORE the
	// listing means the snapshot may include changes newer than the
	// captured value — that only costs a spurious diff round, never a
	// skipped change (a skipped one would need the gate to claim a
	// version newer than the snapshot's content).
	modSeq, gated := uint64(0), false
	if s.modseqGated {
		modSeq, gated = s.srv.Store.MailboxModSeq(context.Background(), s.user, s.mbox)
		if gated && modSeq == s.snapModSeq {
			return nil
		}
	}
	current, err := s.mailboxListAt(context.Background(), s.mbox, s.uidvalidity, modSeq, gated)
	if err != nil {
		if errors.Is(err, mailstore.ErrNotFound) {
			// The selected mailbox vanished from under this session — most
			// often another connection deleted it while the control plane
			// kept this pooled connection alive. Become unselected instead
			// of failing the command that is about to be acknowledged: an
			// error here would close the connection right after the command
			// succeeded, so clients see EOF ("connection closed") for an
			// operation that actually completed.
			s.Unselect()
			return nil
		}
		return err
	}
	old := map[uint32]*mailstore.Message{}
	oldSeq := map[uint32]uint32{}
	for i, m := range s.snap {
		old[m.UID] = m
		oldSeq[m.UID] = uint32(i) + 1
	}
	cur := map[uint32]*mailstore.Message{}
	for _, m := range current {
		cur[m.UID] = m
	}
	// EXISTS when the mailbox grew.
	if uint32(len(current)) > uint32(len(s.snap)) {
		if err := w.WriteNumMessages(uint32(len(current))); err != nil {
			return err
		}
	}
	// FLAGS updates for existing messages.
	for uid, m := range cur {
		o, ok := old[uid]
		if !ok || sameFlags(o, m) {
			continue
		}
		if seq, ok := oldSeq[uid]; ok {
			if err := w.WriteMessageFlags(seq, imap.UID(uid), imapFlags(m.Flags)); err != nil {
				return err
			}
		}
	}
	// EXPUNGEs: RFC 3501 per-message EXPUNGE (descending sequence order),
	// or one ascending VANISHED (RFC 7162) for QRESYNC connections.
	if allowExpunge {
		var removed []uint32
		for uid := range old {
			if _, ok := cur[uid]; !ok {
				removed = append(removed, uid)
			}
		}
		if w.QResyncEnabled() {
			if err := w.WriteVanished(toUIDs(removed)); err != nil {
				return err
			}
		} else {
			sort.Slice(removed, func(i, j int) bool { return oldSeq[removed[i]] > oldSeq[removed[j]] })
			for _, uid := range removed {
				if err := w.WriteExpunge(oldSeq[uid]); err != nil {
					return err
				}
			}
		}
		s.snap = current
		if gated {
			s.snapModSeq = modSeq
		}
		return nil
	}
	// Expunges are not allowed in this round: keep the OLD snapshot. Adopting
	// the new state here would silently swallow the pending expunge and
	// desynchronize the client's sequence numbers — the stale snapshot makes
	// the next diff (and the next allowExpunge round) replay it. Repeated
	// FLAGS/EXISTS re-sends against the stale snapshot are idempotent.
	for uid := range old {
		if _, ok := cur[uid]; !ok {
			return nil
		}
	}
	s.snap = current
	if gated {
		s.snapModSeq = modSeq
	}
	return nil
}

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			if err := s.Poll(w, true); err != nil {
				return err
			}
		}
	}
}

// snapshotMsgs returns the message view that sequence numbers must be
// resolved against: the session snapshot the client has been told about
// (EXISTS/EXPUNGE notifications), falling back to a fresh listing only
// before the first snapshot exists. Resolving seqs against the live list
// while FETCH resolves them against the snapshot let concurrent expunges
// make STORE/MOVE/EXPUNGE act on the WRONG message: the client's "2" is the
// snapshot's 2, not the current list's 2.
func (s *session) snapshotMsgs(ctx context.Context) ([]*mailstore.Message, error) {
	if s.snap != nil {
		return s.snap, nil
	}
	return s.srv.Store.ListMessages(ctx, s.user, s.mbox)
}

// refreshSnapshot re-reads the selected mailbox after commands that changed
// it (STORE/EXPUNGE/MOVE/FETCH-with-\Seen), so Poll does not echo the
// session's own changes back at it.
func (s *session) refreshSnapshot(ctx context.Context) error {
	if s.mbox == "" {
		return nil
	}
	// Version first, listing second (read-order discipline, see Poll).
	modSeq, gated := uint64(0), false
	if s.modseqGated {
		modSeq, gated = s.srv.Store.MailboxModSeq(ctx, s.user, s.mbox)
	}
	msgs, err := s.mailboxListAt(ctx, s.mbox, s.uidvalidity, modSeq, gated)
	if err != nil {
		return err
	}
	s.snap = msgs
	if gated {
		s.snapModSeq = modSeq
	}
	return nil
}

func sameFlags(a, b *mailstore.Message) bool {
	if len(a.Flags) != len(b.Flags) || len(a.Keywords) != len(b.Keywords) {
		return false
	}
	return flagSetEqual(a.Flags, b.Flags) && flagSetEqual(a.Keywords, b.Keywords)
}

// flagSetEqual compares two flag sets without allocating: IMAP flag sets
// are tiny (the five system flags plus a handful of keywords), so a
// contains-scan beats a map in every case that reaches here — and Poll's
// full-mailbox diff reaches here thousands of times per listing.
func flagSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, f := range a {
		if !slices.Contains(b, f) {
			return false
		}
	}
	return true
}

func (s *session) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Delim: mailboxDelim}},
	}, nil
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	ctx := context.Background()
	// Sequence numbers must be relative to the selected snapshot (the
	// client's view), not the live list.
	before, err := s.snapshotMsgs(ctx)
	if err != nil {
		return err
	}
	seqOf := map[uint32]uint32{}
	for i, msg := range before {
		seqOf[msg.UID] = uint32(i) + 1
	}
	var uidList []uint32
	if uids != nil {
		// RFC 4315 §2.1: UID EXPUNGE removes messages that BOTH carry the
		// \Deleted flag AND appear in the given set — the set narrows the
		// candidates, it never bypasses the flag precondition. Test set
		// membership against the UIDs that actually exist instead of
		// expanding the sequence-set: a client may send arbitrary 32-bit
		// ranges ("1:4294967295") and expanding those allocates gigabytes.
		for _, msg := range before {
			if uids.Contains(imap.UID(msg.UID)) && mailstore.HasFlag(msg.Flags, "\\Deleted") {
				uidList = append(uidList, msg.UID)
			}
		}
	}
	deleted, err := s.srv.Store.Expunge(ctx, s.user, s.mbox, uidList)
	if err != nil {
		return err
	}
	if len(deleted) > 0 {
		if err := s.refreshSnapshot(ctx); err != nil {
			return err
		}
		if s.srv.FTS != nil {
			for _, uid := range deleted {
				if err := s.srv.FTS.DeleteMessage(ctx, s.user, s.mbox, uid); err != nil {
					s.srv.Logger.Error("imap: fts delete", "mailbox", s.mbox, "uid", uid, "err", err)
				}
			}
		}
	}
	// RFC 3501: expunge responses in descending sequence order; RFC 7162:
	// one ascending VANISHED for QRESYNC connections.
	if w.QResyncEnabled() {
		return w.WriteVanished(toUIDs(deleted))
	}
	for i := len(deleted) - 1; i >= 0; i-- {
		if seq, ok := seqOf[deleted[i]]; ok {
			if err := w.WriteExpunge(seq); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	ctx := context.Background()
	uids, err := s.resolveUIDs(ctx, numSet)
	if err != nil {
		return nil, err
	}
	mapping, err := s.srv.Store.Copy(ctx, s.user, s.mbox, dest, uids)
	if err != nil {
		if errors.Is(err, mailstore.ErrNotFound) {
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
		}
		return nil, err
	}
	st, err := s.srv.Store.MailboxStatus(ctx, s.user, dest)
	if err != nil {
		return nil, err
	}
	data := &imap.CopyData{UIDValidity: st.UIDValidity}
	for src, dst := range mapping {
		data.SourceUIDs.AddNum(imap.UID(src))
		data.DestUIDs.AddNum(imap.UID(dst))
		s.learnCopyMove(ctx, s.mbox, dest, src, dst)
		s.indexCopyMove(ctx, s.mbox, dest, src, dst, false)
	}
	return data, nil
}

func (s *session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	ctx := context.Background()
	uids, err := s.resolveUIDs(ctx, numSet)
	if err != nil {
		return err
	}
	// Snapshot-based sequence numbers for the EXPUNGE notifications.
	before, err := s.snapshotMsgs(ctx)
	if err != nil {
		return err
	}
	seqOf := map[uint32]uint32{}
	for i, msg := range before {
		seqOf[msg.UID] = uint32(i) + 1
	}
	mapping, err := s.srv.Store.Move(ctx, s.user, s.mbox, dest, uids)
	if err != nil {
		if errors.Is(err, mailstore.ErrNotFound) {
			return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
		}
		return err
	}
	if len(mapping) > 0 {
		if err := s.refreshSnapshot(ctx); err != nil {
			return err
		}
	}
	st, err := s.srv.Store.MailboxStatus(ctx, s.user, dest)
	if err != nil {
		return err
	}
	data := &imap.CopyData{UIDValidity: st.UIDValidity}
	var moved []uint32
	for src, dst := range mapping {
		data.SourceUIDs.AddNum(imap.UID(src))
		data.DestUIDs.AddNum(imap.UID(dst))
		moved = append(moved, src)
		s.learnCopyMove(ctx, s.mbox, dest, src, dst)
		s.indexCopyMove(ctx, s.mbox, dest, src, dst, true)
	}
	if err := w.WriteCopyData(data); err != nil {
		return err
	}
	if w.QResyncEnabled() {
		// RFC 7162: the moved-away source UIDs surface as VANISHED.
		return w.WriteVanished(toUIDs(moved))
	}
	sortUint32(moved)
	for i := len(moved) - 1; i >= 0; i-- {
		if seq, ok := seqOf[moved[i]]; ok {
			if err := w.WriteExpunge(seq); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveUIDs converts a seq/UID set into the concrete UIDs of the selected
// mailbox. Sequence numbers resolve against the session snapshot.
func (s *session) resolveUIDs(ctx context.Context, numSet imap.NumSet) ([]uint32, error) {
	msgs, err := s.snapshotMsgs(ctx)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	var uids []uint32
	for i, msg := range msgs {
		if numMatches(numSet, uint32(i)+1, msg.UID, uint32(len(msgs)), msgs[len(msgs)-1].UID) {
			uids = append(uids, msg.UID)
		}
	}
	return uids, nil
}

func statusData(mb *mailstore.Mailbox, options *imap.StatusOptions) *imap.StatusData {
	data := imap.StatusData{Mailbox: mb.Name}
	if options.NumMessages {
		n := mb.NumMessages
		data.NumMessages = &n
	}
	if options.UIDNext {
		data.UIDNext = imap.UID(mb.UIDNext)
	}
	if options.UIDValidity {
		data.UIDValidity = mb.UIDValidity
	}
	if options.NumUnseen {
		n := mb.NumUnseen
		data.NumUnseen = &n
	}
	if options.NumDeleted {
		n := mb.NumDeleted
		data.NumDeleted = &n
	}
	if options.Size {
		n := mb.Size
		data.Size = &n
	}
	if options.NumRecent {
		n := uint32(0)
		data.NumRecent = &n
	}
	return &data
}

func sortUint32(s []uint32) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
