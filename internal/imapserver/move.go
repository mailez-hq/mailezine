package imapserver

import (
	"github.com/emersion/go-imap/v2"
	"mailezine/internal/imapwire"
)

func (c *Conn) handleMove(dec *imapwire.Decoder, numKind NumKind) error {
	numSet, dest, err := readCopy(numKind, dec)
	if err != nil {
		return err
	}
	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}
	session, ok := c.session.(SessionMove)
	if !ok {
		return newClientBugError("MOVE is not supported")
	}
	w := &MoveWriter{conn: c}
	return session.Move(w, numSet, dest)
}

// MoveWriter writes responses for the MOVE command.
//
// Servers must first call WriteCopyData once, then call WriteExpunge any
// number of times.
type MoveWriter struct {
	conn *Conn
}

// WriteCopyData writes the untagged COPYUID response for a MOVE command.
func (w *MoveWriter) WriteCopyData(data *imap.CopyData) error {
	return w.conn.writeCopyOK("", data)
}

// WriteExpunge writes an EXPUNGE response for a MOVE command.
func (w *MoveWriter) WriteExpunge(seqNum uint32) error {
	return w.conn.writeExpunge(seqNum)
}

// QResyncEnabled reports whether this connection ENABLEd QRESYNC
// (RFC 7162): when true, the move is reported as VANISHED in the source
// mailbox instead of per-message EXPUNGE responses.
func (w *MoveWriter) QResyncEnabled() bool {
	w.conn.mutex.Lock()
	defer w.conn.mutex.Unlock()
	return w.conn.enabled.Has(imap.CapQResync)
}

// WriteVanished writes a VANISHED response for the moved-away source UIDs.
func (w *MoveWriter) WriteVanished(uids []imap.UID) error {
	if len(uids) == 0 {
		return nil
	}
	return w.conn.writeVanished(false, uids)
}
