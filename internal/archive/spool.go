// Package archive captures compliance copies of mail passing the SMTP layer
// (归档). Captures are written durably into the engine's
// KV/blob store first, then a worker forwards them to the control plane;
// a failed forward is retried with backoff instead of being lost.
package archive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"mailezine/internal/mailbuffer"
	"mailezine/internal/stackhttp"
)

const (
	prefixPending = "arch/pending/"
	prefixFailed  = "arch/failed/"
)

// Event is one captured message.
type Event struct {
	Direction    string    `json:"direction"` // inbound | outbound
	EnvelopeFrom string    `json:"envelope_from"`
	EnvelopeTo   []string  `json:"envelope_to"`
	ReceivedAt   time.Time `json:"received_at"`
	Raw          []byte    `json:"-"`
}

// SubmitFunc mirrors the SMTP layer's Submit callback.
type SubmitFunc func(ctx context.Context, peer net.IP, user, from string, to []string, data mailbuffer.Buffer) error

// backingStore is the durable KV/blob surface the spool needs.
type backingStore interface {
	PutRaw(ctx context.Context, key []byte, value []byte) error
	GetRaw(ctx context.Context, key []byte) ([]byte, error)
	DeleteRaw(ctx context.Context, key []byte) error
	ScanRaw(ctx context.Context, prefix []byte, fn func(k, v []byte) error) error
	PutBlob(ctx context.Context, id string, size int64, r io.Reader) (int64, error)
	GetBlob(ctx context.Context, id string, w io.Writer) error
	DeleteBlob(ctx context.Context, id string) error
}

// record is the spooled metadata for one captured message.
type record struct {
	ID           string    `json:"id"`
	BlobID       string    `json:"blob_id"`
	Direction    string    `json:"direction"`
	EnvelopeFrom string    `json:"envelope_from"`
	EnvelopeTo   []string  `json:"envelope_to"`
	ReceivedAt   time.Time `json:"received_at"`
	Attempts     int       `json:"attempts"`
	NextAttempt  time.Time `json:"next_attempt,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

// Spool durably queues captures and forwards them to the control plane.
type Spool struct {
	st          backingStore
	url         string
	hc          *http.Client
	maxAttempts int
	logger      *slog.Logger
	backoffBase time.Duration

	wake    chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup
	started atomic.Bool
}

// New builds a spool. url is the control-plane ingest endpoint; the optional
// secret authenticates the internal API (empty = unauthenticated local dev).
func New(st backingStore, url string, maxAttempts int, logger *slog.Logger, secret ...string) *Spool {
	if logger == nil {
		logger = slog.Default()
	}
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	return &Spool{
		st:          st,
		url:         url,
		hc:          stackhttp.New(stackhttp.First(secret), 30*time.Second),
		maxAttempts: maxAttempts,
		logger:      logger,
		backoffBase: 15 * time.Second,
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
	}
}

// Run starts the forwarding worker. It drains spooled captures left by a
// previous process first, then waits for new captures. Idempotent.
func (sp *Spool) Run(ctx context.Context) {
	if !sp.started.CompareAndSwap(false, true) {
		return
	}
	sp.wg.Add(1)
	go func() {
		defer sp.wg.Done()
		sp.drain(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sp.done:
				return
			case <-sp.wake:
				sp.drain(ctx)
			case <-time.After(30 * time.Second):
				sp.drain(ctx)
			}
		}
	}()
}

// Close stops the worker and waits for the in-flight drain to finish.
func (sp *Spool) Close() {
	if !sp.started.CompareAndSwap(true, false) {
		return
	}
	close(sp.done)
	sp.wg.Wait()
}

// Capture durably spools one event. Never blocks mail flow: the spool write
// is fast and failures are logged (the message itself was already accepted).
func (sp *Spool) Capture(ctx context.Context, ev Event) error {
	id, err := newID()
	if err != nil {
		return err
	}
	// Blob IDs must stay in the safe character set (no path separators):
	// the store rejects "/" to prevent path escape. Use a dash prefix.
	blobID := "arch-" + id
	if _, err := sp.st.PutBlob(ctx, blobID, int64(len(ev.Raw)), bytesReader(ev.Raw)); err != nil {
		return fmt.Errorf("archive: spool blob: %w", err)
	}
	rec := record{
		ID:           id,
		BlobID:       blobID,
		Direction:    ev.Direction,
		EnvelopeFrom: ev.EnvelopeFrom,
		EnvelopeTo:   append([]string(nil), ev.EnvelopeTo...),
		ReceivedAt:   ev.ReceivedAt,
	}
	meta, err := json.Marshal(rec)
	if err != nil {
		_ = sp.st.DeleteBlob(ctx, blobID)
		return err
	}
	if err := sp.st.PutRaw(ctx, []byte(prefixPending+id), meta); err != nil {
		_ = sp.st.DeleteBlob(ctx, blobID)
		return fmt.Errorf("archive: spool meta: %w", err)
	}
	sp.signal()
	return nil
}

// CaptureBuffer spools a mailbuffer (streamed, so large messages never
// round-trip through memory twice).
func (sp *Spool) CaptureBuffer(ctx context.Context, ev Event, buf mailbuffer.Buffer) error {
	id, err := newID()
	if err != nil {
		return err
	}
	// Same safe-ID constraint as Capture: blob IDs must not contain "/".
	blobID := "arch-" + id
	r, err := buf.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := sp.st.PutBlob(ctx, blobID, buf.Len(), r); err != nil {
		return fmt.Errorf("archive: spool blob: %w", err)
	}
	rec := record{
		ID:           id,
		BlobID:       blobID,
		Direction:    ev.Direction,
		EnvelopeFrom: ev.EnvelopeFrom,
		EnvelopeTo:   append([]string(nil), ev.EnvelopeTo...),
		ReceivedAt:   ev.ReceivedAt,
	}
	meta, err := json.Marshal(rec)
	if err != nil {
		_ = sp.st.DeleteBlob(ctx, blobID)
		return err
	}
	if err := sp.st.PutRaw(ctx, []byte(prefixPending+id), meta); err != nil {
		_ = sp.st.DeleteBlob(ctx, blobID)
		return fmt.Errorf("archive: spool meta: %w", err)
	}
	sp.signal()
	return nil
}

// Wrap decorates an SMTP Submit callback so every accepted message is
// captured after the underlying handler returns success.
func (sp *Spool) Wrap(direction string, next SubmitFunc) SubmitFunc {
	return func(ctx context.Context, peer net.IP, user, from string, to []string, data mailbuffer.Buffer) error {
		if err := next(ctx, peer, user, from, to, data); err != nil {
			return err
		}
		if err := sp.CaptureBuffer(ctx, Event{
			Direction:    direction,
			EnvelopeFrom: from,
			EnvelopeTo:   append([]string(nil), to...),
			ReceivedAt:   time.Now(),
		}, data); err != nil {
			// Archive failure must never fail the accepted message; the
			// compliance gap is visible in the logs.
			sp.logger.Error("archive: capture", "direction", direction, "from", from, "err", err)
		}
		return nil
	}
}

func (sp *Spool) signal() {
	select {
	case sp.wake <- struct{}{}:
	default:
	}
}

// drain forwards every due capture. Non-due records are skipped and retried
// on later passes.
func (sp *Spool) drain(ctx context.Context) {
	var keys [][]byte
	err := sp.st.ScanRaw(ctx, []byte(prefixPending), func(k, _ []byte) error {
		keys = append(keys, append([]byte(nil), k...))
		return nil
	})
	if err != nil {
		sp.logger.Error("archive: scan", "err", err)
		return
	}
	now := time.Now()
	for _, key := range keys {
		select {
		case <-ctx.Done():
			return
		default:
		}
		meta, err := sp.st.GetRaw(ctx, key)
		if err != nil {
			continue
		}
		var rec record
		if err := json.Unmarshal(meta, &rec); err != nil {
			sp.logger.Error("archive: corrupt record", "key", string(key), "err", err)
			continue
		}
		if now.Before(rec.NextAttempt) {
			continue
		}
		if err := sp.forwardOne(ctx, key, &rec); err != nil {
			sp.logger.Error("archive: forward", "id", rec.ID, "err", err)
			sp.requeue(ctx, key, &rec, err)
		}
	}
}

func (sp *Spool) forwardOne(ctx context.Context, key []byte, rec *record) error {
	if err := sp.post(ctx, rec); err != nil {
		return err
	}
	// On success the spooled copy is removed.
	if err := sp.st.DeleteRaw(ctx, key); err != nil {
		return fmt.Errorf("delete meta: %w", err)
	}
	if err := sp.st.DeleteBlob(ctx, rec.BlobID); err != nil {
		sp.logger.Warn("archive: delete blob", "id", rec.BlobID, "err", err)
	}
	return nil
}

// post streams one captured message to the control plane as multipart
// (meta JSON + raw RFC 5322 bytes), so large messages never fully load into
// memory.
func (sp *Spool) post(ctx context.Context, rec *record) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		metaPart, err := mw.CreateFormField("meta")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		meta, err := json.Marshal(ingestMeta{
			Direction:    rec.Direction,
			EnvelopeFrom: rec.EnvelopeFrom,
			EnvelopeTo:   rec.EnvelopeTo,
			ReceivedAt:   rec.ReceivedAt,
		})
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := metaPart.Write(meta); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		rawPart, err := mw.CreateFormFile("raw", "message.eml")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := sp.st.GetBlob(ctx, rec.BlobID, rawPart); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("read spooled message: %w", err))
			return
		}
		if err := mw.Close(); err != nil {
			_ = pw.CloseWithError(err)
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sp.url, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := sp.hc.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("archive endpoint status %d", resp.StatusCode)
	}
	return nil
}

// ingestMeta is the JSON metadata part consumed by the control plane.
type ingestMeta struct {
	Direction    string    `json:"direction"`
	EnvelopeFrom string    `json:"envelope_from"`
	EnvelopeTo   []string  `json:"envelope_to"`
	ReceivedAt   time.Time `json:"received_at"`
}

func (sp *Spool) requeue(ctx context.Context, key []byte, rec *record, cause error) {
	rec.Attempts++
	rec.LastError = cause.Error()
	if rec.Attempts >= sp.maxAttempts {
		failedKey := []byte(prefixFailed + rec.ID)
		if meta, err := json.Marshal(rec); err == nil {
			_ = sp.st.PutRaw(ctx, failedKey, meta)
			_ = sp.st.DeleteRaw(ctx, key)
		}
		sp.logger.Error("archive: giving up after retries; copy kept for manual recovery",
			"id", rec.ID, "attempts", rec.Attempts, "err", cause)
		return
	}
	backoff := time.Duration(1<<min(rec.Attempts, 6)) * sp.backoffBase
	rec.NextAttempt = time.Now().Add(backoff)
	if meta, err := json.Marshal(rec); err == nil {
		_ = sp.st.PutRaw(ctx, key, meta)
	}
	// Re-check when the record becomes due.
	time.AfterFunc(backoff, sp.signal)
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// bytesReader adapts a byte slice to io.Reader without keeping a second copy.
func bytesReader(b []byte) io.Reader {
	return &sliceReader{b: b}
}

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
