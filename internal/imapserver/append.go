package imapserver

import (
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/gimap"
	"mailezine/internal/imapwire"
)

// defaultAppendLimit is the default maximum size of an APPEND payload.
const defaultAppendLimit = 100 * 1024 * 1024 // 100MiB

// handleAppendGuarded is wired from the command dispatch; the actual
// EXAMINE check lives inside handleAppend where the mailbox name is known
// and the literal can still be drained before answering.
func (c *Conn) handleAppendGuarded(tag string, dec *imapwire.Decoder) error {
	return c.handleAppend(tag, dec)
}

func (c *Conn) handleAppend(tag string, dec *imapwire.Decoder) error {
	var (
		mailbox string
		options imap.AppendOptions
	)
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		return dec.Err()
	}

	hasFlagList, err := dec.List(func() error {
		flag, err := internal.ExpectFlag(dec)
		if err != nil {
			return err
		}
		options.Flags = append(options.Flags, flag)
		return nil
	})
	if err != nil {
		return err
	}
	if hasFlagList && !dec.ExpectSP() {
		return dec.Err()
	}

	t, err := internal.DecodeDateTime(dec)
	if err != nil {
		return err
	}
	if !t.IsZero() && !dec.ExpectSP() {
		return dec.Err()
	}
	options.Time = t

	var dataExt string
	if !dec.Special('~') && dec.Atom(&dataExt) { // ignore literal8 prefix if any for BINARY
		switch strings.ToUpper(dataExt) {
		case "UTF8":
			// '~' is the literal8 prefix
			if !dec.ExpectSP() || !dec.ExpectSpecial('(') || !dec.ExpectSpecial('~') {
				return dec.Err()
			}
		default:
			return newClientBugError("Unknown APPEND data extension")
		}
	}

	// Check the state before the literal is negotiated, or a pre-auth APPEND
	// earns a continuation and drains its payload first.
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	lit, nonSync, err := dec.ExpectLiteralReader()
	if err != nil {
		return err
	}

	appendLimit := int64(defaultAppendLimit)
	if appendLimitSession, ok := c.session.(SessionAppendLimit); ok {
		appendLimit = int64(appendLimitSession.AppendLimit())
	}

	if lit.Size() > appendLimit {
		// The client's pipelined bytes are still on the wire; answer NO and
		// drop the connection rather than parse them as commands.
		c.state = imap.ConnStateLogout
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: fmt.Sprintf("Literals are limited to %v bytes for this command", appendLimit),
		}
	}
	if err := c.acceptLiteral(lit.Size(), nonSync); err != nil {
		return err
	}

	c.setReadTimeout(literalReadTimeout)
	defer c.setReadTimeout(cmdReadTimeout)

	// EXAMINE write guard: appending into the currently examined mailbox is
	// rejected. The literal is drained first so the wire stays in sync
	// before the NO goes out; other mailboxes stay appendable.
	examined := c.state == imap.ConnStateSelected && c.readOnly && strings.EqualFold(mailbox, c.mbox)

	var data *imap.AppendData
	var appendErr error
	if examined {
		_, _ = io.Copy(io.Discard, lit)
	} else {
		data, appendErr = c.session.Append(mailbox, lit, &options)
	}
	if _, discardErr := io.Copy(io.Discard, lit); discardErr != nil {
		return err
	}
	if dataExt != "" && !dec.ExpectSpecial(')') {
		return dec.Err()
	}
	if !dec.ExpectCRLF() {
		return err
	}
	if examined {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "mailbox is read-only (EXAMINE)",
		}
	}
	if appendErr != nil {
		return appendErr
	}
	if err := c.poll("APPEND"); err != nil {
		return err
	}
	return c.writeAppendOK(tag, data)
}

func (c *Conn) writeAppendOK(tag string, data *imap.AppendData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom(tag).SP().Atom("OK").SP()
	if data != nil {
		enc.Special('[')
		enc.Atom("APPENDUID").SP().Number(data.UIDValidity).SP().UID(data.UID)
		enc.Special(']').SP()
	}
	enc.Text("APPEND completed")
	return enc.CRLF()
}
