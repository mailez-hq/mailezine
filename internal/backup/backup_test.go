package backup

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"mailezine/internal/store"
)

// seedStore populates kv+blob with representative data across spaces: an
// account, a change counter, documents, a blob-link registry, a change-log
// entry and the queue allocator (13 keys plus 2 blobs).
func seedStore(t *testing.T, kv store.KV, blob store.Blob) {
	t.Helper()
	ctx := context.Background()
	seed := []struct {
		k, v []byte
	}{
		{store.MetaNextAccountKey(), []byte{0, 0, 0, 0, 0, 0, 0, 1}},
		{store.MetaEmailKey("ada@example.com"), []byte{0, 0, 0, 1}},
		{store.AccountKey(1), []byte("ada@example.com")},
		{store.CounterKey(1, store.CounterKindChange, nil), []byte{0, 0, 0, 0, 0, 0, 0, 7}},
		{store.CounterKey(1, store.CounterKindNextDoc, []byte{store.CollectionEmail}), []byte{0, 0, 0, 0, 0, 0, 0, 3}},
		{store.FieldKey(1, store.CollectionEmail, 1, store.FieldMeta), []byte{1}},
		{store.FieldKey(1, store.CollectionEmail, 1, 7), []byte{0, 0, 0, 0, 0, 0, 0, 42}},
		{store.ChangeLogKey(1, store.CollectionEmail, 7), []byte{2, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
		{store.BlobLinkKey(1, "email-1-1"), []byte{0, 0, 0, 1}},
		{store.BlobLinkKey(1, "email-1-2"), []byte{0, 0, 0, 1}},
		{store.QueueCounterKey(), []byte{0, 0, 0, 0, 0, 0, 0, 9}},
		{store.QueueKey("outbound", 9), []byte("queued payload")},
		{store.QuotaKey(1), []byte{0, 0, 0, 0, 0, 0, 0, 42}},
	}
	for _, s := range seed {
		if err := kv.Put(s.k, s.v); err != nil {
			t.Fatalf("seed %q: %v", s.k, err)
		}
	}
	for id, body := range map[string]string{
		"email-1-1": "message body one",
		"email-1-2": "message body two with a bit more text",
	} {
		if _, err := blob.Put(ctx, id, int64(len(body)), bytes.NewReader([]byte(body))); err != nil {
			t.Fatalf("seed blob %s: %v", id, err)
		}
	}
}

func dumpKV(t *testing.T, kv store.KV) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := kv.Scan(nil, func(k, v []byte) error {
		out[string(k)] = append([]byte(nil), v...)
		return nil
	})
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	return out
}

func dumpBlobs(t *testing.T, blob store.Blob, ids ...string) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	out := map[string][]byte{}
	for _, id := range ids {
		var buf bytes.Buffer
		if err := blob.Get(ctx, id, &buf); err != nil {
			t.Fatalf("dump blob %s: %v", id, err)
		}
		out[id] = buf.Bytes()
	}
	return out
}

func TestRoundTripMemoryCompressed(t *testing.T) {
	srcKV, srcBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	seedStore(t, srcKV, srcBlob)

	var archive bytes.Buffer
	if _, err := Backup(context.Background(), srcKV, srcBlob, &archive, Options{Compress: true}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	dstKV, dstBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	m, err := Restore(context.Background(), dstKV, dstBlob, bytes.NewReader(archive.Bytes()), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !m.Complete {
		t.Fatal("restore returned an incomplete manifest")
	}
	if m.KVEntries != 13 {
		t.Errorf("kv entries = %d, want 13", m.KVEntries)
	}
	if m.Blobs != 2 {
		t.Errorf("blobs = %d, want 2", m.Blobs)
	}
	if m.Drifted {
		t.Error("drift reported on a quiet store")
	}

	wantKV := dumpKV(t, srcKV)
	if got := dumpKV(t, dstKV); !reflect.DeepEqual(got, wantKV) {
		t.Errorf("kv mismatch: got %d keys, want %d", len(got), len(wantKV))
	}
	wantBlobs := dumpBlobs(t, srcBlob, "email-1-1", "email-1-2")
	if got := dumpBlobs(t, dstBlob, "email-1-1", "email-1-2"); !reflect.DeepEqual(got, wantBlobs) {
		t.Error("blob content mismatch")
	}
}

func TestRoundTripPebble(t *testing.T) {
	srcKV, err := store.OpenPebble(t.TempDir())
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer srcKV.Close()
	srcBlob, err := store.NewFSBlob(t.TempDir())
	if err != nil {
		t.Fatalf("open fsblob: %v", err)
	}
	seedStore(t, srcKV, srcBlob)

	var archive bytes.Buffer
	m1, err := Backup(context.Background(), srcKV, srcBlob, &archive, Options{})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if m1.MissingBlobs != 0 {
		t.Errorf("missing blobs = %d, want 0", m1.MissingBlobs)
	}

	dstKV, err := store.OpenPebble(t.TempDir())
	if err != nil {
		t.Fatalf("open pebble dst: %v", err)
	}
	defer dstKV.Close()
	dstBlob, err := store.NewFSBlob(t.TempDir())
	if err != nil {
		t.Fatalf("open fsblob dst: %v", err)
	}
	if _, err := Restore(context.Background(), dstKV, dstBlob, bytes.NewReader(archive.Bytes()), RestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	wantKV := dumpKV(t, srcKV)
	if got := dumpKV(t, dstKV); !reflect.DeepEqual(got, wantKV) {
		t.Errorf("kv mismatch: got %d keys, want %d", len(got), len(wantKV))
	}
	wantBlobs := dumpBlobs(t, srcBlob, "email-1-1", "email-1-2")
	if got := dumpBlobs(t, dstBlob, "email-1-1", "email-1-2"); !reflect.DeepEqual(got, wantBlobs) {
		t.Error("blob content mismatch")
	}
}

func TestRestoreRejectsTruncated(t *testing.T) {
	srcKV, srcBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	seedStore(t, srcKV, srcBlob)
	var archive bytes.Buffer
	if _, err := Backup(context.Background(), srcKV, srcBlob, &archive, Options{}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	cut := archive.Bytes()[:archive.Len()-10]
	dstKV, dstBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	if _, err := Restore(context.Background(), dstKV, dstBlob, bytes.NewReader(cut), RestoreOptions{}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestRestoreCorruptPayloadDetected(t *testing.T) {
	srcKV, srcBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	seedStore(t, srcKV, srcBlob)
	var archive bytes.Buffer
	if _, err := Backup(context.Background(), srcKV, srcBlob, &archive, Options{}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Flip one payload byte (inside the last KV value) keeping the length.
	raw := archive.Bytes()
	raw[len(raw)-40] ^= 0xff
	dstKV, dstBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	if _, err := Restore(context.Background(), dstKV, dstBlob, bytes.NewReader(raw), RestoreOptions{}); err == nil {
		t.Fatal("corrupted payload accepted")
	}
}

func TestRestoreEmptyTargetRequired(t *testing.T) {
	srcKV, srcBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	seedStore(t, srcKV, srcBlob)
	var archive bytes.Buffer
	if _, err := Backup(context.Background(), srcKV, srcBlob, &archive, Options{}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	dstKV, dstBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	if err := dstKV.Put([]byte("pre-existing"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), dstKV, dstBlob, bytes.NewReader(archive.Bytes()), RestoreOptions{})
	if err == nil {
		t.Fatal("restore into non-empty target accepted without -overwrite")
	}

	if _, err := Restore(context.Background(), dstKV, dstBlob, bytes.NewReader(archive.Bytes()), RestoreOptions{Overwrite: true}); err != nil {
		t.Fatalf("merge restore: %v", err)
	}
	got := dumpKV(t, dstKV)
	if string(got["pre-existing"]) != "value" {
		t.Error("merge lost pre-existing key")
	}
	if _, ok := got[string(store.AccountKey(1))]; !ok {
		t.Error("merge lost archive key")
	}
}

func TestBackupMissingBlobCounted(t *testing.T) {
	kv, blob := store.NewMemoryKV(), store.NewMemoryBlob()
	if err := kv.Put(store.BlobLinkKey(1, "ghost"), []byte{0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	m, err := Backup(context.Background(), kv, blob, &archive, Options{})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if m.MissingBlobs != 1 {
		t.Errorf("missing blobs = %d, want 1", m.MissingBlobs)
	}

	dstKV, dstBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	if _, err := Restore(context.Background(), dstKV, dstBlob, bytes.NewReader(archive.Bytes()), RestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := dumpKV(t, dstKV); len(got) != 1 {
		t.Errorf("restored %d keys, want 1", len(got))
	}
}

func TestRestoreVerifyOnlyWritesNothing(t *testing.T) {
	srcKV, srcBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	seedStore(t, srcKV, srcBlob)
	var archive bytes.Buffer
	if _, err := Backup(context.Background(), srcKV, srcBlob, &archive, Options{Compress: true}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	dstKV, dstBlob := store.NewMemoryKV(), store.NewMemoryBlob()
	if _, err := Restore(context.Background(), dstKV, dstBlob, bytes.NewReader(archive.Bytes()), RestoreOptions{VerifyOnly: true}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := dumpKV(t, dstKV); len(got) != 0 {
		t.Errorf("verify wrote %d keys", len(got))
	}
}

func TestRestoreBadMagic(t *testing.T) {
	kv, blob := store.NewMemoryKV(), store.NewMemoryBlob()
	_, err := Restore(context.Background(), kv, blob, bytes.NewReader([]byte("not a backup at all")), RestoreOptions{})
	if err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestBackupContextCancelled(t *testing.T) {
	kv, blob := store.NewMemoryKV(), store.NewMemoryBlob()
	seedStore(t, kv, blob)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var archive bytes.Buffer
	if _, err := Backup(ctx, kv, blob, &archive, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestFramingByteRoundTrip guards the raw record framing (no gzip) so a
// future reader can rely on the exact byte layout documented in the package
// comment.
func TestFramingByteRoundTrip(t *testing.T) {
	kv, blob := store.NewMemoryKV(), store.NewMemoryBlob()
	seedStore(t, kv, blob)
	var archive bytes.Buffer
	if _, err := Backup(context.Background(), kv, blob, &archive, Options{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	raw := archive.Bytes()
	if string(raw[:len(magic)]) != magic {
		t.Fatalf("magic = %q, want %q", raw[:len(magic)], magic)
	}
	// Header, then records; find the end marker and manifest type byte.
	if raw[len(magic)] != recKV {
		t.Fatalf("first record type = %q, want %q", raw[len(magic)], recKV)
	}
	if !bytes.Contains(raw, []byte{recEnd, recManifest}) {
		t.Fatal("end marker not followed by manifest record")
	}
}
