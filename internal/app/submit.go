package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"

	"mailezine/internal/delivery"
	"mailezine/internal/directory"
	"mailezine/internal/mailbuffer"
	"mailezine/internal/queue"
)

// submitHandler is the SMTP layer's Submit callback shape. It is a type
// alias (not a definition) so every named use stays assignable to the raw
// signature; the compliance seam wraps it per edition.
type submitHandler = func(ctx context.Context, peer net.IP, user, from string, to []string, data mailbuffer.Buffer) error

// submitSigner signs locally delivered submissions (best effort). A nil
// implementation means "no DKIM vault configured; deliver unsigned".
type submitSigner interface {
	Sign(ctx context.Context, from string, msg []byte) ([]byte, error)
}

// newSubmit routes envelope recipients: local addresses go through the
// delivery pipeline; external addresses are spooled for relay when the
// outbound queue is enabled. Locally delivered submissions are DKIM-signed
// too — the message originates from our domain, so the stored copy carries
// the same signature an external relay would get.
func newSubmit(dir directory.Service, pipeline *delivery.Pipeline, qm *queue.Manager, signer submitSigner, logger *slog.Logger) submitHandler {
	return func(ctx context.Context, peer net.IP, user, from string, to []string, data mailbuffer.Buffer) error {
		// Outbound mail never carries internal Received chains or client
		// fingerprints collected on the way in. The filter streams so large
		// messages never round-trip through memory twice.
		clean, err := cleanOutbound(data)
		if err != nil {
			return fmt.Errorf("smtp: outclean: %w", err)
		}
		defer clean.Remove()
		var local, relay []string
		for _, rcpt := range to {
			targets, aerr := dir.Aliases(ctx, rcpt)
			if aerr == nil && len(targets) > 0 {
				local = append(local, rcpt)
			} else if qm != nil {
				relay = append(relay, rcpt)
			} else {
				return fmt.Errorf("smtp: outbound relay disabled for %s", rcpt)
			}
		}
		if len(local) > 0 {
			raw, err := clean.ReadAll()
			if err != nil {
				return err
			}
			if signer != nil {
				if signed, serr := signer.Sign(ctx, from, raw); serr == nil {
					raw = signed
				} else {
					// Signing is best effort (opportunisticSigner already
					// degrades vault outages); never block local delivery.
					logger.Warn("smtp: local sign skipped", "from", from, "err", serr)
				}
			}
			if err := pipeline.Deliver(ctx, peer, from, local, raw); err != nil {
				return err
			}
		}
		if len(relay) > 0 {
			// SRS: rewrite the envelope sender when relaying mail that did
			// not originate locally, so bounces route back through us.
			relayFrom := from
			if user == "" || user != from {
				if rewritten, err := dir.SRSForward(ctx, from); err == nil && rewritten != "" {
					relayFrom = rewritten
				} else if err != nil && !errors.Is(err, directory.ErrNotFound) {
					logger.Warn("smtp: srs forward", "from", from, "err", err)
				}
			}
			subj, err := subjectOfReader(clean)
			if err != nil {
				return err
			}
			body, err := clean.Open()
			if err != nil {
				return err
			}
			if _, err := qm.Submit(ctx, relayFrom, relay, subj, body); err != nil {
				return err
			}
			logger.Info("queued outbound", "from", relayFrom, "to", relay, "bytes", clean.Len())
		}
		return nil
	}
}

// cleanOutbound streams the outbound privacy filter into a fresh buffer.
func cleanOutbound(data mailbuffer.Buffer) (mailbuffer.Buffer, error) {
	in, err := data.Open()
	if err != nil {
		return nil, err
	}
	defer in.Close()
	out, err := os.CreateTemp("", "mailezine-out-*")
	if err != nil {
		return nil, err
	}
	if err := delivery.OutcleanTo(in, out); err != nil {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return nil, err
	}
	return mailbuffer.FromFile(out)
}

// subjectOf extracts the first Subject header from message bytes (used by
// the Sieve-redirect path, which already holds the message in memory).
func subjectOf(data []byte) string {
	for _, line := range strings.Split(string(data), "\r\n") {
		if line == "" {
			break // end of headers
		}
		if v, ok := strings.CutPrefix(line, "Subject:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// subjectOfReader extracts the first Subject header for queue metadata.
func subjectOfReader(buf mailbuffer.Buffer) (string, error) {
	r, err := buf.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()
	sc := bufio.NewReader(r)
	for {
		line, err := sc.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return "", nil // end of headers
		}
		if v, ok := strings.CutPrefix(line, "Subject:"); ok {
			return strings.TrimSpace(v), nil
		}
		if err == io.EOF {
			return "", nil
		}
	}
}
