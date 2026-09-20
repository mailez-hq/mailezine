package imapserver

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"mailezine/internal/imapwire"
)

const (
	cmdReadTimeout     = 30 * time.Second
	idleReadTimeout    = 35 * time.Minute // section 5.4 says 30min minimum
	literalReadTimeout = 5 * time.Minute

	respWriteTimeout    = 30 * time.Second
	literalWriteTimeout = 5 * time.Minute

	maxCommandSize = 50 * 1024 // RFC 2683 section 3.2.1.5 says 8KiB minimum
)

var internalServerErrorResp = &imap.StatusResponse{
	Type: imap.StatusResponseTypeNo,
	Code: imap.ResponseCodeServerBug,
	Text: "Internal server error",
}

// A Conn represents an IMAP connection to the server.
type Conn struct {
	server   *Server
	br       *bufio.Reader
	bw       *bufio.Writer
	encMutex sync.Mutex

	mutex   sync.Mutex
	conn    net.Conn
	enabled imap.CapSet

	state   imap.ConnState
	session Session
	// readOnly remembers EXAMINE for the current selection: mutating
	// commands (STORE/COPY/MOVE/EXPUNGE, CLOSE's implicit expunge, APPEND
	// into the examined mailbox) are rejected with NO while it holds.
	// Commands are dispatched from a single goroutine, so the plain field
	// needs no lock.
	readOnly bool
	mbox     string
}

func newConn(c net.Conn, server *Server) *Conn {
	rw := server.options.wrapReadWriter(c)
	br := bufio.NewReader(rw)
	bw := bufio.NewWriter(rw)
	return &Conn{
		conn:    c,
		server:  server,
		br:      br,
		bw:      bw,
		enabled: make(imap.CapSet),
	}
}

// NetConn returns the underlying connection that is wrapped by the IMAP
// connection.
//
// Writing to or reading from this connection directly will corrupt the IMAP
// session.
func (c *Conn) NetConn() net.Conn {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.conn
}

// Bye terminates the IMAP connection.
func (c *Conn) Bye(text string) error {
	respErr := c.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeBye,
		Text: text,
	})
	closeErr := c.conn.Close()
	if respErr != nil {
		return respErr
	}
	return closeErr
}

func (c *Conn) EnabledCaps() imap.CapSet {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.enabled.Copy()
}

func (c *Conn) serve() {
	defer func() {
		if v := recover(); v != nil {
			c.server.logger().Printf("panic handling command: %v\n%s", v, debug.Stack())
		}

		c.conn.Close()
	}()

	c.server.mutex.Lock()
	c.server.conns[c] = struct{}{}
	c.server.mutex.Unlock()
	defer func() {
		c.server.mutex.Lock()
		delete(c.server.conns, c)
		c.server.mutex.Unlock()
	}()

	var (
		greetingData *GreetingData
		err          error
	)
	c.session, greetingData, err = c.server.options.NewSession(c)
	if err != nil {
		var (
			resp    *imap.StatusResponse
			imapErr *imap.Error
		)
		if errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeBye {
			resp = (*imap.StatusResponse)(imapErr)
		} else {
			c.server.logger().Printf("failed to create session: %v", err)
			resp = internalServerErrorResp
		}
		if err := c.writeStatusResp("", resp); err != nil {
			c.server.logger().Printf("failed to write greeting: %v", err)
		}
		return
	}

	defer func() {
		if c.session != nil {
			if err := c.session.Close(); err != nil {
				c.server.logger().Printf("failed to close session: %v", err)
			}
		}
	}()

	caps := c.server.options.caps()
	if _, ok := c.session.(SessionIMAP4rev2); !ok && caps.Has(imap.CapIMAP4rev2) {
		panic("imapserver: server advertises IMAP4rev2 but session doesn't support it")
	}
	if _, ok := c.session.(SessionNamespace); !ok && caps.Has(imap.CapNamespace) {
		panic("imapserver: server advertises NAMESPACE but session doesn't support it")
	}
	if _, ok := c.session.(SessionMove); !ok && caps.Has(imap.CapMove) {
		panic("imapserver: server advertises MOVE but session doesn't support it")
	}
	if _, ok := c.session.(SessionUnauthenticate); !ok && caps.Has(imap.CapUnauthenticate) {
		panic("imapserver: server advertises UNAUTHENTICATE but session doesn't support it")
	}

	c.state = imap.ConnStateNotAuthenticated
	statusType := imap.StatusResponseTypeOK
	if greetingData != nil && greetingData.PreAuth {
		c.state = imap.ConnStateAuthenticated
		statusType = imap.StatusResponseTypePreAuth
	}
	if err := c.writeCapabilityStatus("", statusType, "IMAP server ready"); err != nil {
		c.server.logger().Printf("failed to write greeting: %v", err)
		return
	}

	for {
		var readTimeout time.Duration
		switch c.state {
		case imap.ConnStateAuthenticated, imap.ConnStateSelected:
			readTimeout = idleReadTimeout
		default:
			readTimeout = cmdReadTimeout
		}
		c.setReadTimeout(readTimeout)

		dec := imapwire.NewDecoder(c.br, imapwire.ConnSideServer)
		dec.MaxSize = maxCommandSize
		dec.CheckBufferedLiteralFunc = c.checkBufferedLiteral

		if c.state == imap.ConnStateLogout || dec.EOF() {
			break
		}

		c.setReadTimeout(cmdReadTimeout)
		if err := c.readCommand(dec); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				c.server.logger().Printf("failed to read command: %v", err)
			}
			break
		}
	}
}

// rejectIfReadOnly returns a tagged NO response when the current selection
// came from EXAMINE: RFC 3501 §6.3.2 — no mutation may be performed on an
// examined mailbox.
func (c *Conn) rejectIfReadOnly(tag string) error {
	if c.state == imap.ConnStateSelected && c.readOnly {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "mailbox is read-only (EXAMINE)",
		}
	}
	return nil
}

func (c *Conn) readCommand(dec *imapwire.Decoder) error {
	var tag, name string
	if !dec.ExpectAtom(&tag) || !dec.ExpectSP() || !dec.ExpectAtom(&name) {
		return fmt.Errorf("in command: %w", dec.Err())
	}
	name = strings.ToUpper(name)

	numKind := NumKindSeq
	if name == "UID" {
		numKind = NumKindUID
		var subName string
		if !dec.ExpectSP() || !dec.ExpectAtom(&subName) {
			return fmt.Errorf("in command: %w", dec.Err())
		}
		name = "UID " + strings.ToUpper(subName)
	}

	// TODO: handle multiple commands concurrently
	sendOK := true
	var err error
	switch name {
	case "NOOP", "CHECK":
		err = c.handleNoop(dec)
	case "LOGOUT":
		err = c.handleLogout(dec)
	case "CAPABILITY":
		err = c.handleCapability(dec)
	case "ID":
		err = c.handleID(dec)
	case "STARTTLS":
		err = c.handleStartTLS(tag, dec)
		sendOK = false
	case "AUTHENTICATE":
		err = c.handleAuthenticate(tag, dec)
		sendOK = false
	case "UNAUTHENTICATE":
		err = c.handleUnauthenticate(dec)
	case "LOGIN":
		err = c.handleLogin(tag, dec)
		sendOK = false
	case "ENABLE":
		err = c.handleEnable(dec)
	case "CREATE":
		err = c.handleCreate(dec)
	case "DELETE":
		err = c.handleDelete(dec)
	case "RENAME":
		err = c.handleRename(dec)
	case "SUBSCRIBE":
		err = c.handleSubscribe(dec)
	case "UNSUBSCRIBE":
		err = c.handleUnsubscribe(dec)
	case "STATUS":
		err = c.handleStatus(dec)
	case "LIST":
		err = c.handleList(dec)
	case "LSUB":
		err = c.handleLSub(dec)
	case "NAMESPACE":
		err = c.handleNamespace(dec)
	case "IDLE":
		err = c.handleIdle(dec)
	case "SELECT", "EXAMINE":
		err = c.handleSelect(tag, dec, name == "EXAMINE")
		sendOK = false
	case "CLOSE", "UNSELECT":
		// CLOSE on an examined mailbox behaves like UNSELECT: the implicit
		// expunge would be a write (RFC 3501 §6.4.2).
		err = c.handleUnselect(dec, name == "CLOSE" && !c.readOnly)
	case "APPEND":
		err = c.handleAppendGuarded(tag, dec)
		sendOK = false
	case "FETCH", "UID FETCH":
		err = c.handleFetch(dec, numKind)
	case "EXPUNGE":
		if e := c.rejectIfReadOnly(tag); e != nil {
			err = e
			break
		}
		err = c.handleExpunge(dec)
	case "UID EXPUNGE":
		if e := c.rejectIfReadOnly(tag); e != nil {
			err = e
			break
		}
		err = c.handleUIDExpunge(dec)
	case "STORE", "UID STORE":
		if e := c.rejectIfReadOnly(tag); e != nil {
			err = e
			break
		}
		err = c.handleStore(dec, numKind)
	case "COPY", "UID COPY":
		if e := c.rejectIfReadOnly(tag); e != nil {
			err = e
			break
		}
		err = c.handleCopy(tag, dec, numKind)
		sendOK = false
	case "MOVE", "UID MOVE":
		if e := c.rejectIfReadOnly(tag); e != nil {
			err = e
			break
		}
		err = c.handleMove(dec, numKind)
	case "SEARCH", "UID SEARCH":
		err = c.handleSearch(tag, dec, numKind)
	case "SORT", "UID SORT":
		err = c.handleSort(tag, dec, numKind)
	default:
		// Extension commands (e.g. RFC 4314 ACL) are handled by the
		// session's extension hook; unknown commands still get the
		// standard BAD/bye treatment below.
		if ext, ok := c.session.(SessionExtension); ok && (c.state == imap.ConnStateAuthenticated || c.state == imap.ConnStateSelected) {
			w := NewExtensionWriter(c)
			handled, extErr := ext.HandleExtension(name, dec, w)
			w.End() // releases the response encoder lock
			if handled {
				err = extErr
				break
			}
		}
		if c.state == imap.ConnStateNotAuthenticated {
			// Don't allow a single unknown command before authentication to
			// mitigate cross-protocol attacks:
			// https://www-archive.mozilla.org/projects/netlib/portbanning
			c.state = imap.ConnStateLogout
			defer c.Bye("Unknown command")
		}
		err = &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Unknown command",
		}
	}

	dec.DiscardLine()

	var (
		resp    *imap.StatusResponse
		imapErr *imap.Error
		decErr  *imapwire.DecoderExpectError
	)
	if okCode, isOK := asOKCode(err); isOK {
		if !sendOK {
			return nil
		}
		if err := c.poll(name); err != nil {
			return err
		}
		return c.writeOKCodeResp(tag, okCode, name)
	}
	if errors.As(err, &imapErr) {
		resp = (*imap.StatusResponse)(imapErr)
	} else if errors.As(err, &decErr) {
		resp = &imap.StatusResponse{
			Type: imap.StatusResponseTypeBad,
			Code: imap.ResponseCodeClientBug,
			Text: "Syntax error: " + decErr.Message,
		}
	} else if err != nil {
		c.server.logger().Printf("handling %v command: %v", name, err)
		resp = internalServerErrorResp
	} else {
		if !sendOK {
			return nil
		}
		if err := c.poll(name); err != nil {
			return err
		}
		resp = &imap.StatusResponse{
			Type: imap.StatusResponseTypeOK,
			Text: fmt.Sprintf("%v completed", name),
		}
	}
	return c.writeStatusResp(tag, resp)
}

func (c *Conn) handleNoop(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	return nil
}

func (c *Conn) handleLogout(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	c.state = imap.ConnStateLogout

	return c.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeBye,
		Text: "Logging out",
	})
}

func (c *Conn) handleDelete(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Delete(name)
}

func (c *Conn) handleRename(dec *imapwire.Decoder) error {
	var oldName, newName string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&oldName) || !dec.ExpectSP() || !dec.ExpectMailbox(&newName) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	var options imap.RenameOptions
	return c.session.Rename(oldName, newName, &options)
}

func (c *Conn) handleSubscribe(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Subscribe(name)
}

func (c *Conn) handleUnsubscribe(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Unsubscribe(name)
}

func (c *Conn) checkBufferedLiteral(size int64, nonSync bool) error {
	if size > 4096 {
		// Pipelined payload is still on the wire after a rejected literal;
		// answer NO and drop the connection (RFC 7888 §4).
		c.state = imap.ConnStateLogout
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: "Literals are limited to 4096 bytes for this command",
		}
	}

	return c.acceptLiteral(size, nonSync)
}

func (c *Conn) acceptLiteral(size int64, nonSync bool) error {
	if nonSync && size > 4096 && !c.server.options.caps().Has(imap.CapLiteralPlus) {
		c.state = imap.ConnStateLogout
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Non-synchronizing literals are limited to 4096 bytes",
		}
	}

	if nonSync {
		return nil
	}

	return c.writeContReq("Ready for literal data")
}

func (c *Conn) canAuth() bool {
	if c.state != imap.ConnStateNotAuthenticated {
		return false
	}
	_, isTLS := c.conn.(*tls.Conn)
	return isTLS || c.server.options.InsecureAuth
}

func (c *Conn) writeStatusResp(tag string, statusResp *imap.StatusResponse) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeStatusResp(enc.Encoder, tag, statusResp)
}

func (c *Conn) writeContReq(text string) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeContReq(enc.Encoder, text)
}

func (c *Conn) writeCapabilityStatus(tag string, typ imap.StatusResponseType, text string) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeCapabilityStatus(enc.Encoder, tag, typ, c.availableCaps(), text)
}

func (c *Conn) checkState(state imap.ConnState) error {
	if state == imap.ConnStateAuthenticated && c.state == imap.ConnStateSelected {
		return nil
	}
	if c.state != state {
		return newClientBugError(fmt.Sprintf("This command is only valid in the %s state", state))
	}
	return nil
}

func (c *Conn) setReadTimeout(dur time.Duration) {
	if dur > 0 {
		c.conn.SetReadDeadline(time.Now().Add(dur))
	} else {
		c.conn.SetReadDeadline(time.Time{})
	}
}

func (c *Conn) setWriteTimeout(dur time.Duration) {
	if dur > 0 {
		c.conn.SetWriteDeadline(time.Now().Add(dur))
	} else {
		c.conn.SetWriteDeadline(time.Time{})
	}
}

func (c *Conn) poll(cmd string) error {
	switch c.state {
	case imap.ConnStateAuthenticated, imap.ConnStateSelected:
		// nothing to do
	default:
		return nil
	}

	allowExpunge := true
	switch cmd {
	case "FETCH", "STORE", "SEARCH":
		allowExpunge = false
	}

	w := &UpdateWriter{conn: c, allowExpunge: allowExpunge}
	return c.session.Poll(w, allowExpunge)
}

type responseEncoder struct {
	*imapwire.Encoder
	conn *Conn
}

func newResponseEncoder(conn *Conn) *responseEncoder {
	conn.mutex.Lock()
	quotedUTF8 := conn.enabled.Has(imap.CapIMAP4rev2) || conn.enabled.Has(imap.CapUTF8Accept)
	conn.mutex.Unlock()

	wireEnc := imapwire.NewEncoder(conn.bw, imapwire.ConnSideServer)
	wireEnc.QuotedUTF8 = quotedUTF8

	conn.encMutex.Lock() // released by responseEncoder.end
	conn.setWriteTimeout(respWriteTimeout)
	return &responseEncoder{
		Encoder: wireEnc,
		conn:    conn,
	}
}

func (enc *responseEncoder) end() {
	if enc.Encoder == nil {
		panic("imapserver: responseEncoder.end called twice")
	}
	enc.Encoder = nil
	enc.conn.setWriteTimeout(0)
	enc.conn.encMutex.Unlock()
}

func (enc *responseEncoder) Literal(size int64) io.WriteCloser {
	enc.conn.setWriteTimeout(literalWriteTimeout)
	return literalWriter{
		WriteCloser: enc.Encoder.Literal(size, nil),
		conn:        enc.conn,
	}
}

type literalWriter struct {
	io.WriteCloser
	conn *Conn
}

func (lw literalWriter) Close() error {
	lw.conn.setWriteTimeout(respWriteTimeout)
	return lw.WriteCloser.Close()
}

func writeStatusResp(enc *imapwire.Encoder, tag string, statusResp *imap.StatusResponse) error {
	if tag == "" {
		tag = "*"
	}
	enc.Atom(tag).SP().Atom(string(statusResp.Type)).SP()
	if statusResp.Code != "" {
		enc.Atom(fmt.Sprintf("[%v]", statusResp.Code)).SP()
	}
	enc.Text(statusResp.Text)
	return enc.CRLF()
}

func writeCapabilityOK(enc *imapwire.Encoder, tag string, caps []imap.Cap, text string) error {
	return writeCapabilityStatus(enc, tag, imap.StatusResponseTypeOK, caps, text)
}

func writeCapabilityStatus(enc *imapwire.Encoder, tag string, typ imap.StatusResponseType, caps []imap.Cap, text string) error {
	if tag == "" {
		tag = "*"
	}

	enc.Atom(tag).SP().Atom(string(typ)).SP().Special('[').Atom("CAPABILITY")
	for _, c := range caps {
		enc.SP().Atom(string(c))
	}
	enc.Special(']').SP().Text(text)
	return enc.CRLF()
}

func writeContReq(enc *imapwire.Encoder, text string) error {
	return enc.Atom("+").SP().Text(text).CRLF()
}

func newClientBugError(text string) error {
	return &imap.Error{
		Type: imap.StatusResponseTypeBad,
		Code: imap.ResponseCodeClientBug,
		Text: text,
	}
}

// UpdateWriter writes status updates.
type UpdateWriter struct {
	conn         *Conn
	allowExpunge bool
}

// QResyncEnabled reports whether this connection ENABLEd QRESYNC
// (RFC 7162): when true, expunges are reported as VANISHED responses
// instead of per-message EXPUNGE responses.
func (w *UpdateWriter) QResyncEnabled() bool {
	w.conn.mutex.Lock()
	defer w.conn.mutex.Unlock()
	return w.conn.enabled.Has(imap.CapQResync)
}

// WriteVanished writes a VANISHED response for the given UIDs (ascending
// order on the wire).
func (w *UpdateWriter) WriteVanished(uids []imap.UID) error {
	if len(uids) == 0 {
		return nil
	}
	return w.conn.writeVanished(false, uids)
}

// WriteExpunge writes an EXPUNGE response.
func (w *UpdateWriter) WriteExpunge(seqNum uint32) error {
	if !w.allowExpunge {
		return fmt.Errorf("imapserver: EXPUNGE updates are not allowed in this context")
	}
	return w.conn.writeExpunge(seqNum)
}

// WriteNumMessages writes an EXISTS response.
func (w *UpdateWriter) WriteNumMessages(n uint32) error {
	return w.conn.writeExists(n)
}

// WriteNumRecent writes an RECENT response (not used in IMAP4rev2, will be ignored).
func (w *UpdateWriter) WriteNumRecent(n uint32) error {
	if w.conn.enabled.Has(imap.CapIMAP4rev2) || !w.conn.server.options.caps().Has(imap.CapIMAP4rev1) {
		return nil
	}
	return w.conn.writeObsoleteRecent(n)
}

// WriteMailboxFlags writes a FLAGS response.
func (w *UpdateWriter) WriteMailboxFlags(flags []imap.Flag) error {
	return w.conn.writeFlags(flags)
}

// writeVanished writes a VANISHED response (RFC 7162 §3.2.10). With
// earlier=true the "(EARLIER)" variant used during SELECT resynchronization
// is emitted. UIDs are sorted ascending and compressed into ranges.
func (c *Conn) writeVanished(earlier bool, uids []imap.UID) error {
	sorted := append([]imap.UID(nil), uids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	set := uidRangesString(sorted)
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("VANISHED")
	if earlier {
		enc.SP().Special('(').Atom("EARLIER").Special(')')
	}
	enc.SP().Atom(set)
	return enc.CRLF()
}

// uidRangesString renders ascending UIDs as an IMAP sequence-set string,
// compressing consecutive values into "a:b" ranges.
func uidRangesString(uids []imap.UID) string {
	if len(uids) == 0 {
		return ""
	}
	var b strings.Builder
	start := uids[0]
	prev := uids[0]
	flush := func() {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		if start == prev {
			fmt.Fprintf(&b, "%d", start)
		} else {
			fmt.Fprintf(&b, "%d:%d", start, prev)
		}
	}
	for _, u := range uids[1:] {
		if u == prev+1 {
			prev = u
			continue
		}
		flush()
		start = u
		prev = u
	}
	flush()
	return b.String()
}

// WriteMessageFlags writes a FETCH response with FLAGS.
func (w *UpdateWriter) WriteMessageFlags(seqNum uint32, uid imap.UID, flags []imap.Flag) error {
	fetchWriter := &FetchWriter{conn: w.conn}
	respWriter := fetchWriter.CreateMessage(seqNum)
	if uid != 0 {
		respWriter.WriteUID(uid)
	}
	respWriter.WriteFlags(flags)
	return respWriter.Close()
}
