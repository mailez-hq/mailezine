// IMAP QUOTA extension (RFC 2087): GETQUOTA / GETQUOTAROOT expose the
// account quota. Data comes from the directory (quota limit) and the store
// (used bytes).
package imap

import (
	"context"

	"github.com/emersion/go-imap/v2"

	"mailezine/internal/imapserver"
	"mailezine/internal/imapwire"
)

func (s *session) handleQuota(name string, dec *imapwire.Decoder, w *imapserver.ExtensionWriter) (bool, error) {
	switch name {
	case "GETQUOTA", "GETQUOTAROOT":
	default:
		return false, nil
	}
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectAString(&mailbox) || !dec.ExpectCRLF() {
		return true, dec.Err()
	}
	used, limit, err := s.quotaValues(context.Background())
	if err != nil {
		return true, err
	}
	if name == "GETQUOTAROOT" {
		w.Atom("*").SP().Atom("QUOTAROOT").SP().Mailbox(mailbox).SP().Quoted("")
		if err := w.CRLF(); err != nil {
			return true, err
		}
	}
	w.Atom("*").SP().Atom("QUOTA").SP().Quoted("").SP()
	le := w.BeginList()
	le.Item().Atom("STORAGE").SP().Number64(used).SP().Number64(limit)
	le.End()
	return true, w.CRLF()
}

func (s *session) quotaValues(ctx context.Context) (used, limit int64, err error) {
	u, err := s.srv.Directory.User(ctx, s.user)
	if err != nil {
		return 0, 0, err
	}
	used, err = s.srv.Store.QuotaUsedBytes(ctx, s.user)
	if err != nil {
		return 0, 0, err
	}
	return used, u.QuotaBytes, nil
}

var _ = imap.CapQuota
