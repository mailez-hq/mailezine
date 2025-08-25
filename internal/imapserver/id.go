// RFC 2971 ID extension: exchange client/server identification. The server
// answers with its own identity regardless of what the client sent.
package imapserver

import (
	"mailezine/internal/imapwire"
	"mailezine/internal/version"
)

func (c *Conn) handleID(dec *imapwire.Decoder) error {
	// RFC 2971 §3.2: the client sends NIL or a parenthesized list of
	// key/value string pairs. Both forms are accepted and discarded.
	if dec.SP() {
		if dec.ExpectNIL() {
			// no client fields
		} else if _, err := dec.List(func() error {
			var k, v string
			if !dec.ExpectString(&k) || !dec.SP() || !dec.ExpectString(&v) {
				return dec.Err()
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	enc := newResponseEncoder(c)
	defer enc.end()
	le := enc.Atom("*").SP().Atom("ID").SP().BeginList()
	le.Item().Quoted("name").SP().Quoted("mailezine")
	le.Item().Quoted("version").SP().Quoted(version.Version)
	le.Item().Quoted("vendor").SP().Quoted("mailez")
	le.End()
	return enc.CRLF()
}
