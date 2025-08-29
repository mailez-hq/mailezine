package imapserver

import (
	"errors"
	"fmt"

	"github.com/emersion/go-imap/v2"
)

// ResponseCodeModified is the CONDSTORE response code for STORE UNCHANGEDSINCE
// conflicts (RFC 7162 §3.3): the tagged OK carries
// [MODIFIED <sequence-set>] listing messages skipped because their modseq
// exceeded the requested UNCHANGEDSINCE value.
const ResponseCodeModified imap.ResponseCode = "MODIFIED"

// StatusOKCode lets a command handler attach a response code (with an
// optional sequence-set payload) to the tagged OK of a successfully handled
// command. The handler returns it instead of nil; execute() writes the
// completion status.
type StatusOKCode struct {
	Code imap.ResponseCode
	Set  imap.NumSet // optional sequence-set written after the code
}

func (e *StatusOKCode) Error() string { return string(e.Code) }

// asOKCode reports whether err carries tagged-OK response data.
func asOKCode(err error) (*StatusOKCode, bool) {
	var ok *StatusOKCode
	return ok, errors.As(err, &ok)
}

// writeOKCodeResp writes the tagged OK with the bracketed response code and
// its payload.
func (c *Conn) writeOKCodeResp(tag string, ok *StatusOKCode, name string) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	if tag == "" {
		tag = "*"
	}
	enc.Atom(tag).SP().Atom(string(imap.StatusResponseTypeOK)).SP()
	enc.Special('[').Atom(string(ok.Code))
	if ok.Set != nil {
		enc.SP().NumSet(ok.Set)
	}
	enc.Special(']').SP().Text(fmt.Sprintf("%v completed", name))
	return enc.CRLF()
}
