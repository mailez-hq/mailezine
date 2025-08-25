// FETCH implementation: metadata + body sections, with automatic \Seen on
// non-PEEK body fetches (RFC 3501 semantics).
package imap

import (
	"bufio"
	"bytes"
	"context"
	"io"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/textproto"
	"mailezine/internal/imapserver"

	"mailezine/internal/mailstore"
)

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	ctx := context.Background()
	msgs, err := s.srv.Store.ListMessages(ctx, s.user, s.mbox)
	if err != nil {
		return err
	}
	markSeen := false
	changed := false
	for _, bs := range options.BodySection {
		if !bs.Peek {
			markSeen = true
			break
		}
	}
	for i, msg := range msgs {
		seq := uint32(i) + 1
		if !numContains(numSet, seq, msg.UID) {
			continue
		}
		rc, err := s.srv.Store.OpenMessage(ctx, s.user, s.mbox, msg.UID)
		if err != nil {
			return err
		}
		buf, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		if markSeen && !mailstore.HasFlag(msg.Flags, "\\Seen") {
			flags := append([]string(nil), msg.Flags...)
			flags = append(flags, "\\Seen")
			if err := s.srv.Store.SetFlags(ctx, s.user, s.mbox, msg.UID, flags); err != nil {
				return err
			}
			msg.Flags = flags
			changed = true
		}

		rw := w.CreateMessage(seq)
		rw.WriteUID(imap.UID(msg.UID))
		if options.ModSeq {
			rw.WriteModSeq(msg.ModSeq)
		}
		if options.Flags {
			rw.WriteFlags(imapFlags(msg.Flags))
		}
		if options.InternalDate {
			rw.WriteInternalDate(msg.InternalDate)
		}
		if options.RFC822Size {
			rw.WriteRFC822Size(int64(len(buf)))
		}
		if options.Envelope {
			rw.WriteEnvelope(envelopeOf(buf))
		}
		if options.BodyStructure != nil {
			rw.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(buf)))
		}
		for _, bs := range options.BodySection {
			section := imapserver.ExtractBodySection(bytes.NewReader(buf), bs)
			wc := rw.WriteBodySection(bs, int64(len(section)))
			if _, err := wc.Write(section); err != nil {
				_ = rw.Close()
				return err
			}
			if err := wc.Close(); err != nil {
				_ = rw.Close()
				return err
			}
		}
		for _, bs := range options.BinarySection {
			section := imapserver.ExtractBinarySection(bytes.NewReader(buf), bs)
			wc := rw.WriteBinarySection(bs, int64(len(section)))
			if _, err := wc.Write(section); err != nil {
				_ = rw.Close()
				return err
			}
			if err := wc.Close(); err != nil {
				_ = rw.Close()
				return err
			}
		}
		for _, bss := range options.BinarySectionSize {
			rw.WriteBinarySectionSize(bss, imapserver.ExtractBinarySectionSize(bytes.NewReader(buf), bss))
		}
		if err := rw.Close(); err != nil {
			return err
		}
	}
	if changed {
		return s.refreshSnapshot(ctx)
	}
	return nil
}

// numContains resolves a seq/UID set against one message.
func numContains(numSet imap.NumSet, seq uint32, uid uint32) bool {
	switch ns := numSet.(type) {
	case imap.SeqSet:
		return ns.Contains(seq)
	case imap.UIDSet:
		return ns.Contains(imap.UID(uid))
	}
	return false
}

func imapFlags(flags []string) []imap.Flag {
	out := make([]imap.Flag, 0, len(flags))
	for _, f := range flags {
		out = append(out, imap.Flag(f))
	}
	return out
}

func envelopeOf(buf []byte) *imap.Envelope {
	br := bufio.NewReader(bytes.NewReader(buf))
	header, err := textproto.ReadHeader(br)
	if err != nil {
		return nil
	}
	return imapserver.ExtractEnvelope(header)
}
