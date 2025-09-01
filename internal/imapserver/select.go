package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/imapwire"
)

func (c *Conn) handleSelect(tag string, dec *imapwire.Decoder, readOnly bool) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) {
		return dec.Err()
	}
	options := imap.SelectOptions{ReadOnly: readOnly}
	var qresync *QRESYNCParam
	if dec.SP() {
		// RFC 7162: SELECT ... (CONDSTORE) and
		// SELECT ... (QRESYNC (uidvalidity modseq [known-uids]))
		if _, err := dec.List(func() error {
			var name string
			if !dec.ExpectAtom(&name) {
				return dec.Err()
			}
			if strings.EqualFold(name, "CONDSTORE") {
				options.CondStore = true
				return nil
			}
			if strings.EqualFold(name, "QRESYNC") {
				p, err := readQRESYNCParam(dec)
				if err != nil {
					return err
				}
				qresync = p
				// RFC 7162 §3.2.5: the parameter implies CONDSTORE.
				options.CondStore = true
				return nil
			}
			return dec.Err()
		}); err != nil {
			return err
		}
		if !dec.ExpectCRLF() {
			return dec.Err()
		}
	} else if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if qresync != nil {
		c.mutex.Lock()
		enabled := c.enabled.Has(imap.CapQResync)
		c.mutex.Unlock()
		if !enabled {
			return newClientBugError("QRESYNC parameter requires ENABLE QRESYNC first")
		}
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	if c.state == imap.ConnStateSelected {
		if err := c.session.Unselect(); err != nil {
			return err
		}
		c.state = imap.ConnStateAuthenticated
		err := c.writeStatusResp("", &imap.StatusResponse{
			Type: imap.StatusResponseTypeOK,
			Code: "CLOSED",
			Text: "Previous mailbox is now closed",
		})
		if err != nil {
			return err
		}
	}

	var (
		data  *imap.SelectData
		qdata *QResyncData
		err   error
	)
	if qresync != nil {
		qs, ok := c.session.(SessionQRESYNC)
		if !ok {
			return newClientBugError("QRESYNC is not supported by this backend")
		}
		data, qdata, err = qs.SelectQRESYNC(mailbox, &options, *qresync, &UpdateWriter{conn: c, allowExpunge: true})
	} else {
		data, err = c.session.Select(mailbox, &options)
	}
	if err != nil {
		return err
	}

	if err := c.writeExists(data.NumMessages); err != nil {
		return err
	}
	if !c.enabled.Has(imap.CapIMAP4rev2) && c.server.options.caps().Has(imap.CapIMAP4rev1) {
		if err := c.writeObsoleteRecent(data.NumRecent); err != nil {
			return err
		}
		if data.FirstUnseenSeqNum != 0 {
			if err := c.writeObsoleteUnseen(data.FirstUnseenSeqNum); err != nil {
				return err
			}
		}
	}
	if err := c.writeUIDValidity(data.UIDValidity); err != nil {
		return err
	}
	if err := c.writeUIDNext(data.UIDNext); err != nil {
		return err
	}
	if err := c.writeFlags(data.Flags); err != nil {
		return err
	}
	if err := c.writePermanentFlags(data.PermanentFlags); err != nil {
		return err
	}
	if options.CondStore || qresync != nil {
		// RFC 7162 §3.2: HIGHESTMODSEQ response code when CONDSTORE is
		// requested (or NOMODSEQ when the mailbox has no modseq support).
		enc := newResponseEncoder(c)
		enc.Atom("*").SP().Atom("OK").SP().Special('[').Atom("HIGHESTMODSEQ").SP().ModSeq(data.HighestModSeq).Special(']').SP().Text("Highest")
		if err := enc.CRLF(); err != nil {
			return err
		}
		enc.end()
	}
	if qdata != nil {
		// RFC 7162 §3.2.5.2: resynchronization payload between the standard
		// untagged responses and the tagged OK. VANISHED (EARLIER) first,
		// then the flag updates; skipped entirely on UIDVALIDITY mismatch
		// (the client's cache is void anyway).
		if qresync.UIDValidity == data.UIDValidity {
			if len(qdata.VanishedEarlier) > 0 {
				if err := c.writeVanished(true, qdata.VanishedEarlier); err != nil {
					return err
				}
			}
			for _, u := range qdata.FlagUpdates {
				fw := &FetchWriter{conn: c}
				mw := fw.CreateMessage(u.SeqNum)
				mw.WriteUID(u.UID)
				if u.ModSeq != 0 {
					mw.WriteModSeq(u.ModSeq)
				}
				mw.WriteFlags(u.Flags)
				if err := mw.Close(); err != nil {
					return err
				}
			}
		}
	}
	if data.List != nil {
		if err := c.writeList(data.List); err != nil {
			return err
		}
	}

	c.state = imap.ConnStateSelected
	// Remember the selection mode for the dispatch-level write guard
	// (EXAMINE must be read-only end to end).
	c.readOnly = readOnly
	c.mbox = mailbox

	var (
		cmdName string
		code    imap.ResponseCode
	)
	if readOnly {
		cmdName = "EXAMINE"
		code = "READ-ONLY"
	} else {
		cmdName = "SELECT"
		code = "READ-WRITE"
	}
	return c.writeStatusResp(tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Code: code,
		Text: fmt.Sprintf("%v completed", cmdName),
	})
}

func (c *Conn) handleUnselect(dec *imapwire.Decoder, expunge bool) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}

	if expunge {
		w := &ExpungeWriter{}
		if err := c.session.Expunge(w, nil); err != nil {
			return err
		}
	}

	if err := c.session.Unselect(); err != nil {
		return err
	}

	c.state = imap.ConnStateAuthenticated
	c.readOnly = false
	c.mbox = ""
	return nil
}

// readQRESYNCParam parses the RFC 7162 §3.2.5 parameter body after the
// QRESYNC atom: SP "(uidvalidity modseq [known-uids [seq-match-data]])".
func readQRESYNCParam(dec *imapwire.Decoder) (*QRESYNCParam, error) {
	p := &QRESYNCParam{}
	if !dec.ExpectSP() || !dec.ExpectSpecial('(') {
		return nil, dec.Err()
	}
	if !dec.ExpectNumber(&p.UIDValidity) || !dec.ExpectSP() || !dec.ExpectModSeq(&p.ModSeq) {
		return nil, dec.Err()
	}
	if dec.SP() {
		var known imap.NumSet
		if !dec.ExpectNumSet(NumKindUID.wire(), &known) {
			return nil, dec.Err()
		}
		p.KnownUIDs = known
		// Optional seq-match-data: "(known-sequence-set known-uid-set)". It
		// is a client-convenience hint for narrowing EXPUNGE replays; the
		// authoritative resynchronization payload is computed from the
		// tombstone log, so we only validate the syntax here.
		if dec.SP() && dec.Special('(') {
			var seqSet, matchUIDs imap.NumSet
			if !dec.ExpectNumSet(NumKindSeq.wire(), &seqSet) || !dec.ExpectSP() ||
				!dec.ExpectNumSet(NumKindUID.wire(), &matchUIDs) || !dec.ExpectSpecial(')') {
				return nil, dec.Err()
			}
		}
	}
	if !dec.ExpectSpecial(')') {
		return nil, dec.Err()
	}
	return p, nil
}

func (c *Conn) writeExists(numMessages uint32) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return enc.Atom("*").SP().Number(numMessages).SP().Atom("EXISTS").CRLF()
}

func (c *Conn) writeObsoleteRecent(n uint32) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return enc.Atom("*").SP().Number(n).SP().Atom("RECENT").CRLF()
}

func (c *Conn) writeObsoleteUnseen(n uint32) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("UNSEEN").SP().Number(n).Special(']')
	enc.SP().Text("First unseen message")
	return enc.CRLF()
}

func (c *Conn) writeUIDValidity(uidValidity uint32) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("UIDVALIDITY").SP().Number(uidValidity).Special(']')
	enc.SP().Text("UIDs valid")
	return enc.CRLF()
}

func (c *Conn) writeUIDNext(uidNext imap.UID) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("UIDNEXT").SP().UID(uidNext).Special(']')
	enc.SP().Text("Predicted next UID")
	return enc.CRLF()
}

func (c *Conn) writeFlags(flags []imap.Flag) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("FLAGS").SP().List(len(flags), func(i int) {
		enc.Flag(flags[i])
	})
	return enc.CRLF()
}

func (c *Conn) writePermanentFlags(flags []imap.Flag) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("PERMANENTFLAGS").SP().List(len(flags), func(i int) {
		enc.Flag(flags[i])
	}).Special(']')
	enc.SP().Text("Permanent flags")
	return enc.CRLF()
}
