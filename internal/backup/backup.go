// Package backup implements streaming full-archive export and import of the
// mailezine store: every KV key across all ARCHITECTURE.md §3.2 spaces plus
// every blob referenced by the blob-link registry. Archives are a single
// self-describing stream so they can be piped into gzip, S3 or tape without
// intermediate files.
//
// Stream layout (optionally gzip-wrapped as a whole):
//
//	magic "MZBACK\x01"
//	records until the end marker, each introduced by a type byte:
//	  'k'  u32 klen | key | u32 vlen | value          (KV entry)
//	  'b'  u32 idlen | id | u64 size | size bytes     (blob)
//	  'x'                                             (end of data)
//	  'm'  u32 len | manifest JSON                    (after 'x' only)
//
// The SHA-256 in the manifest covers every byte from after the magic through
// the 'x' marker inclusive, so truncation and bit rot are detected on
// restore. The manifest is written last and carries a Complete flag, so an
// interrupted backup produces a file that restore always rejects.
//
// Consistency: the scan is not a point-in-time snapshot. Backup records the
// per-account change counters and the queue counter before and after the
// walk and flags drift in the log — for a consistent archive, stop the
// server (or the HA active node) first. A drifted archive is still usable:
// it simply mixes a slightly newer queue/log state into an older document
// state, which the crash-consistency rules of §3.5 already tolerate.
package backup

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"mailezine/internal/store"
	"mailezine/internal/version"
)

const magic = "MZBACK\x01" // 7 bytes

// Record type bytes (see package comment).
const (
	recKV       byte = 'k'
	recBlob     byte = 'b'
	recEnd      byte = 'x'
	recManifest byte = 'm'
)

// Format identifies the archive kind in the manifest.
const Format = "mailezine-backup"

// ErrTruncated is returned when a stream ends before the end marker.
var ErrTruncated = errors.New("backup: archive is truncated (missing end marker)")

// Manifest describes one archive. It is written as the final record and is
// not covered by the payload checksum.
type Manifest struct {
	Format       string    `json:"format"`
	Version      int       `json:"version"`
	Engine       string    `json:"engine"`
	CreatedAt    time.Time `json:"created_at"`
	Backend      string    `json:"backend,omitempty"`
	KVEntries    int64     `json:"kv_entries"`
	KVBytes      int64     `json:"kv_bytes"`
	Blobs        int64     `json:"blobs"`
	BlobBytes    int64     `json:"blob_bytes"`
	MissingBlobs int64     `json:"missing_blobs,omitempty"`
	Drifted      bool      `json:"drifted,omitempty"`
	Complete     bool      `json:"complete"`
	SHA256       string    `json:"sha256"`
}

// Options configures Backup.
type Options struct {
	// Backend labels the source in the manifest (informational).
	Backend string
	// Compress wraps the whole archive in gzip.
	Compress bool
	// Logger receives progress lines; nil disables logging.
	Logger *slog.Logger
}

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// Overwrite allows restoring into a non-empty target (merge: keys and
	// blobs from the archive are upserted, extra target data is kept).
	Overwrite bool
	// VerifyOnly reads and checks the archive without writing anything.
	VerifyOnly bool
	// Logger receives progress lines; nil disables logging.
	Logger *slog.Logger
}

// Backup streams a full archive of kv+blob into out and returns the
// manifest. On error the returned manifest is incomplete and out contains a
// truncated stream that Restore rejects.
func Backup(ctx context.Context, kv store.KV, blob store.Blob, out io.Writer, opts Options) (Manifest, error) {
	m := Manifest{
		Format:    Format,
		Version:   1,
		Engine:    version.String(),
		CreatedAt: time.Now().UTC(),
		Backend:   opts.Backend,
	}

	bw := bufio.NewWriterSize(out, 1<<20)
	var dst io.Writer = bw
	var gz *gzip.Writer
	if opts.Compress {
		gz = gzip.NewWriter(bw)
		dst = gz
	}
	if _, err := dst.Write([]byte(magic)); err != nil {
		return m, finishWrite(bw, gz, err)
	}

	h := sha256.New()
	hw := io.MultiWriter(dst, h)

	before, err := snapshotCounters(kv)
	if err != nil {
		return m, finishWrite(bw, gz, err)
	}

	// KV pass: every key of every space, ascending.
	err = kv.Scan(nil, func(k, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeKVEntry(hw, k, v); err != nil {
			return err
		}
		m.KVEntries++
		m.KVBytes += int64(len(k) + len(v))
		if opts.Logger != nil && m.KVEntries%50000 == 0 {
			opts.Logger.Info("backup: kv", "entries", m.KVEntries, "bytes", m.KVBytes)
		}
		return nil
	})
	if err != nil {
		return m, finishWrite(bw, gz, err)
	}

	// Blob pass: every blob referenced by the blob-link registry. Blobs
	// with no link left are orphans the GC owns — they are skipped (and
	// counted) rather than archived.
	ids, err := linkedBlobIDs(kv)
	if err != nil {
		return m, finishWrite(bw, gz, err)
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return m, finishWrite(bw, gz, err)
		}
		size, err := blob.Stat(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			m.MissingBlobs++
			if opts.Logger != nil {
				opts.Logger.Warn("backup: missing blob (referenced but absent)", "id", id)
			}
			continue
		}
		if err != nil {
			return m, finishWrite(bw, gz, err)
		}
		if err := writeBlobHeader(hw, id, size); err != nil {
			return m, finishWrite(bw, gz, err)
		}
		cw := &countingWriter{w: hw}
		if err := blob.Get(ctx, id, cw); err != nil {
			return m, finishWrite(bw, gz, fmt.Errorf("backup: blob %s: %w", id, err))
		}
		if cw.n != size {
			return m, finishWrite(bw, gz, fmt.Errorf("backup: blob %s: size drift (stat %d, read %d)", id, size, cw.n))
		}
		m.Blobs++
		m.BlobBytes += size
		if opts.Logger != nil && m.Blobs%1000 == 0 {
			opts.Logger.Info("backup: blobs", "blobs", m.Blobs, "bytes", m.BlobBytes)
		}
	}

	after, err := snapshotCounters(kv)
	if err != nil {
		return m, finishWrite(bw, gz, err)
	}
	m.Drifted = !countersEqual(before, after)
	if m.Drifted && opts.Logger != nil {
		opts.Logger.Warn("backup: store changed during the walk (write activity detected); " +
			"the archive is eventually consistent — stop the server for a point-in-time backup")
	}

	// End marker participates in the checksum; manifest does not.
	if _, err := hw.Write([]byte{recEnd}); err != nil {
		return m, finishWrite(bw, gz, err)
	}
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	m.Complete = true

	raw, err := json.Marshal(m)
	if err != nil {
		return m, finishWrite(bw, gz, err)
	}
	if err := writeManifest(dst, raw); err != nil {
		return m, finishWrite(bw, gz, err)
	}
	return m, finishWrite(bw, gz, nil)
}

// Restore reads an archive from in into kv+blob and returns the manifest
// recorded inside it. The payload checksum and the entry counts are verified
// before success is reported.
func Restore(ctx context.Context, kv store.KV, blob store.Blob, in io.Reader, opts RestoreOptions) (Manifest, error) {
	var zero Manifest
	br := bufio.NewReaderSize(in, 1<<20)
	src := io.Reader(br)
	if head, _ := br.Peek(2); len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return zero, err
		}
		defer gz.Close()
		src = gz
	}

	var magicBuf [len(magic)]byte
	if _, err := io.ReadFull(src, magicBuf[:]); err != nil {
		return zero, fmt.Errorf("backup: %w", err)
	}
	if string(magicBuf[:]) != magic {
		return zero, errors.New("backup: not a mailezine backup (bad magic)")
	}

	if !opts.Overwrite && !opts.VerifyOnly {
		if err := ensureEmpty(kv); err != nil {
			return zero, err
		}
	}

	h := sha256.New()
	tr := io.TeeReader(src, h) // framing bytes and blob bodies all flow through here

	var (
		m         Manifest
		kvCount   int64
		blobCount int64
		blobBytes int64
		ops       []store.Op
	)
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		if err := kv.Batch(ops); err != nil {
			return err
		}
		ops = ops[:0]
		return nil
	}

	for {
		var typ [1]byte
		if _, err := io.ReadFull(tr, typ[:]); err != nil {
			return zero, truncated(err)
		}
		switch typ[0] {
		case recKV:
			k, v, err := readKVEntry(tr)
			if err != nil {
				return zero, err
			}
			kvCount++
			if opts.VerifyOnly {
				continue
			}
			ops = append(ops, store.Op{Key: k, Value: v})
			if len(ops) >= 512 {
				if err := flush(); err != nil {
					return zero, err
				}
			}
			if opts.Logger != nil && kvCount%50000 == 0 {
				opts.Logger.Info("restore: kv", "entries", kvCount)
			}
		case recBlob:
			id, size, err := readBlobHeader(tr)
			if err != nil {
				return zero, err
			}
			blobCount++
			if opts.VerifyOnly {
				// tr already feeds the checksum; just drain the body.
				if _, err := io.CopyN(io.Discard, tr, size); err != nil {
					return zero, truncated(err)
				}
				blobBytes += size
				continue
			}
			// The body is hashed exactly once, whether or not the backend
			// consumes it: content-addressed stores may skip the read when
			// the blob already exists, so whatever Put leaves unread is
			// drained through the hash below.
			hr := &hashingReader{r: io.LimitReader(src, size), h: h}
			if _, err := blob.Put(ctx, id, size, hr); err != nil {
				return zero, fmt.Errorf("backup: blob %s: %w", id, err)
			}
			if hr.n < size {
				if _, err := io.CopyN(io.Discard, hr, size-hr.n); err != nil {
					return zero, truncated(err)
				}
			}
			blobBytes += size
			if opts.Logger != nil && blobCount%1000 == 0 {
				opts.Logger.Info("restore: blobs", "blobs", blobCount, "bytes", blobBytes)
			}
		case recEnd:
			raw, err := readManifest(src) // manifest is outside the checksum
			if err != nil {
				return zero, err
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				return zero, fmt.Errorf("backup: bad manifest: %w", err)
			}
			if err := flush(); err != nil {
				return zero, err
			}
			if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
				return zero, fmt.Errorf("backup: checksum mismatch (manifest %s, got %s)", m.SHA256, got)
			}
			if kvCount != m.KVEntries || blobCount != m.Blobs || blobBytes != m.BlobBytes {
				return zero, fmt.Errorf(
					"backup: entry count mismatch (manifest kv=%d blobs=%d bytes=%d, got kv=%d blobs=%d bytes=%d)",
					m.KVEntries, m.Blobs, m.BlobBytes, kvCount, blobCount, blobBytes)
			}
			if !m.Complete {
				return zero, errors.New("backup: archive was not completed by backup (interrupted run)")
			}
			return m, nil
		default:
			return zero, fmt.Errorf("backup: corrupt archive (unknown record type 0x%02x)", typ[0])
		}
	}
}

// ensureEmpty rejects a restore into a store that already holds data, unless
// the caller opted into merge semantics via Overwrite.
func ensureEmpty(kv store.KV) error {
	errNonEmpty := errors.New("not empty")
	err := kv.Scan(nil, func(k, v []byte) error { return errNonEmpty })
	switch {
	case errors.Is(err, errNonEmpty):
		return errors.New("backup: target store is not empty; use -overwrite to merge the archive into it")
	case err != nil:
		return err
	}
	return nil
}

// snapshotCounters captures the per-account change counters and the queue
// allocator so Backup can detect write activity during the walk.
func snapshotCounters(kv store.KV) (map[string]string, error) {
	snap := map[string]string{}
	err := kv.Scan([]byte{store.SpaceAccount}, func(k, v []byte) error {
		// Counter keys: 'a' | account(4) | kind(1) [| sub]. The change
		// counter (kind 0x02, no sub) moves on every mutation.
		if len(k) == 6 && k[5] == store.CounterKindChange {
			snap[string(k)] = string(v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if v, err := kv.Get(store.QueueCounterKey()); err == nil {
		snap["\x00queue-counter"] = string(v)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return snap, nil
}

func countersEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// linkedBlobIDs returns the sorted unique blob IDs referenced by the
// blob-link registry. Link keys are 'b' | account(4) | blobID.
func linkedBlobIDs(kv store.KV) ([]string, error) {
	seen := map[string]struct{}{}
	err := kv.Scan([]byte{store.SpaceBlobLink}, func(k, _ []byte) error {
		if len(k) > 5 {
			seen[string(k[5:])] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func writeKVEntry(w io.Writer, k, v []byte) error {
	var hdr [9]byte
	hdr[0] = recKV
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(k)))
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(v)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(k); err != nil {
		return err
	}
	_, err := w.Write(v)
	return err
}

// readKVEntry parses a KV record body (the type byte is already consumed).
func readKVEntry(r io.Reader) ([]byte, []byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, nil, truncated(err)
	}
	k := make([]byte, binary.BigEndian.Uint32(hdr[0:4]))
	v := make([]byte, binary.BigEndian.Uint32(hdr[4:8]))
	if _, err := io.ReadFull(r, k); err != nil {
		return nil, nil, truncated(err)
	}
	if _, err := io.ReadFull(r, v); err != nil {
		return nil, nil, truncated(err)
	}
	return k, v, nil
}

func writeBlobHeader(w io.Writer, id string, size int64) error {
	var hdr [13]byte
	hdr[0] = recBlob
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(id)))
	binary.BigEndian.PutUint64(hdr[5:13], uint64(size))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, id)
	return err
}

// readBlobHeader parses a blob record header (the type byte is already
// consumed) and returns the id and the framed body size.
func readBlobHeader(r io.Reader) (string, int64, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", 0, truncated(err)
	}
	id := make([]byte, binary.BigEndian.Uint32(hdr[0:4]))
	if _, err := io.ReadFull(r, id); err != nil {
		return "", 0, truncated(err)
	}
	return string(id), int64(binary.BigEndian.Uint64(hdr[4:12])), nil
}

func writeManifest(w io.Writer, raw []byte) error {
	var hdr [5]byte
	hdr[0] = recManifest
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(raw)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(raw)
	return err
}

// readManifest parses a manifest record, whose five-byte header (the 'm'
// type byte plus the length) is read straight from r — the type byte is
// outside the checksum, so it must not flow through the caller's TeeReader.
func readManifest(r io.Reader) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, truncated(err)
	}
	if hdr[0] != recManifest {
		return nil, fmt.Errorf("backup: corrupt archive (manifest record type 0x%02x)", hdr[0])
	}
	raw := make([]byte, binary.BigEndian.Uint32(hdr[1:5]))
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, truncated(err)
	}
	return raw, nil
}

// finishWrite closes the gzip layer (flushing it) and then the bufio
// writer, so even error paths leave a parseable truncated stream behind.
func finishWrite(bw *bufio.Writer, gz *gzip.Writer, err error) error {
	if gz != nil {
		_ = gz.Close()
	}
	_ = bw.Flush()
	return err
}

func truncated(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	}
	return err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// hashingReader feeds every byte read into h and counts them, so Restore
// can tell how much of a blob body the backend actually consumed.
type hashingReader struct {
	r io.Reader
	h io.Writer // the sha256 digest; avoids depending on crypto/hash
	n int64
}

func (hr *hashingReader) Read(p []byte) (int, error) {
	n, err := hr.r.Read(p)
	if n > 0 {
		hr.h.Write(p[:n])
		hr.n += int64(n)
	}
	return n, err
}
