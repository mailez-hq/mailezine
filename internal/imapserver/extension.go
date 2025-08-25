// Extension mechanism for IMAP commands not covered by the go-imap/v2
// imapserver session interface (e.g. RFC 4314 ACL). mailezine vendors this
// package to add the hook without forking the upstream wire protocol.
package imapserver

import "mailezine/internal/imapwire"

// SessionExtension is implemented by sessions that handle extension
// commands. The Conn dispatches unknown commands to the extension before
// rejecting them, so protocol additions (ACL, etc.) stay outside the core
// dispatch table.
type SessionExtension interface {
	// HandleExtension parses and executes one extension command (name is
	// normalized, e.g. "GETACL"; "UID FOO" arrives as "UID FOO"). The
	// handler must consume the full command line including CRLF. It returns
	// handled=true when it recognized the command; the Conn then completes
	// with the usual tagged OK/BAD/NO response.
	HandleExtension(name string, dec *imapwire.Decoder, w *ExtensionWriter) (handled bool, err error)
}

// ExtensionWriter writes untagged responses for extension commands. It wraps
// the connection response encoder; End must be called after the response.
type ExtensionWriter struct {
	enc *responseEncoder
}

// NewExtensionWriter acquires the connection's response encoder.
func NewExtensionWriter(c *Conn) *ExtensionWriter {
	return &ExtensionWriter{enc: newResponseEncoder(c)}
}

// Atom writes an atom.
func (w *ExtensionWriter) Atom(s string) *ExtensionWriter {
	w.enc.Atom(s)
	return w
}

// SP writes a space.
func (w *ExtensionWriter) SP() *ExtensionWriter {
	w.enc.SP()
	return w
}

// Mailbox writes a mailbox name.
func (w *ExtensionWriter) Mailbox(name string) *ExtensionWriter {
	w.enc.Mailbox(name)
	return w
}

// Quoted writes a quoted string.
func (w *ExtensionWriter) Quoted(s string) *ExtensionWriter {
	w.enc.Quoted(s)
	return w
}

// NIL writes the nil literal.
func (w *ExtensionWriter) NIL() *ExtensionWriter {
	w.enc.NIL()
	return w
}

// String writes a quoted or literal string.
func (w *ExtensionWriter) String(s string) *ExtensionWriter {
	w.enc.String(s)
	return w
}

// CRLF terminates the response line.
func (w *ExtensionWriter) CRLF() error {
	return w.enc.CRLF()
}

// BeginList starts a parenthesized list; the returned encoder writes items
// separated by spaces and must be finished with End.
func (w *ExtensionWriter) BeginList() *imapwire.ListEncoder {
	return w.enc.BeginList()
}

// End releases the connection write lock.
func (w *ExtensionWriter) End() {
	w.enc.end()
}
