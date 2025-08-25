// FSBlob stores immutable blobs as files under a root directory. Writes are
// atomic (temp file + fsync + rename) and idempotent for content-addressed
// IDs; IDs are restricted to a safe character set so untrusted input can
// never escape the root (ARCHITECTURE.md §8.2).
package store

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// FSBlob implements Blob on the local filesystem.
type FSBlob struct {
	root       string
	compressed bool
}

// NewFSBlob opens (creating if needed) a blob directory.
func NewFSBlob(root string) (*FSBlob, error) {
	return newFSBlob(root, false)
}

// NewFSBlobCompressed stores blobs gzip-compressed at rest. Reading a
// previously stored uncompressed blob is still supported (gzip magic
// detection).
func NewFSBlobCompressed(root string) (*FSBlob, error) {
	return newFSBlob(root, true)
}

func newFSBlob(root string, compressed bool) (*FSBlob, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &FSBlob{root: root, compressed: compressed}, nil
}

func (b *FSBlob) Put(_ context.Context, id string, size int64, r io.Reader) (int64, error) {
	if err := validateBlobID(id); err != nil {
		return 0, err
	}
	path := filepath.Join(b.root, id)
	if fi, err := os.Stat(path); err == nil {
		_ = fi
		return size, nil // content-addressed: same id ⇒ same content
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	tmp, err := os.CreateTemp(b.root, ".tmp-*")
	if err != nil {
		return 0, err
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	if b.compressed {
		gz := gzip.NewWriter(tmp)
		if _, err := io.Copy(gz, r); err != nil {
			cleanup()
			return 0, err
		}
		if err := gz.Close(); err != nil {
			cleanup()
			return 0, err
		}
	} else {
		if _, err := io.Copy(tmp, r); err != nil {
			cleanup()
			return 0, err
		}
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		if errors.Is(err, os.ErrExist) {
			// Lost a race with an identical content-addressed write.
			if fi, err2 := os.Stat(path); err2 == nil {
				return fi.Size(), nil
			}
		}
		return 0, err
	}
	return size, nil
}

func (b *FSBlob) Get(_ context.Context, id string, w io.Writer) error {
	if err := validateBlobID(id); err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(b.root, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	defer f.Close()
	if b.compressed {
		// gzip magic detection keeps uncompressed blobs (written before
		// compression was enabled, or migrated maildir data) readable.
		var magic [2]byte
		if _, err := io.ReadFull(f, magic[:]); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			gz, err := gzip.NewReader(f)
			if err != nil {
				return err
			}
			defer gz.Close()
			_, err = io.Copy(w, gz)
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	_, err = io.Copy(w, f)
	return err
}

func (b *FSBlob) Delete(_ context.Context, id string) error {
	if err := validateBlobID(id); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(b.root, id))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func (b *FSBlob) Stat(_ context.Context, id string) (int64, error) {
	if err := validateBlobID(id); err != nil {
		return 0, err
	}
	fi, err := os.Stat(filepath.Join(b.root, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return fi.Size(), nil
}

// validateBlobID restricts IDs to a safe, portable character set and rejects
// anything that could traverse directories (path safety invariant INV-FS).
func validateBlobID(id string) error {
	if id == "" || id == "." || id == ".." || len(id) > 128 {
		return errors.New("store: invalid blob id")
	}
	for _, c := range id {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.'
		if !ok {
			return errors.New("store: invalid blob id")
		}
	}
	return nil
}

var _ Blob = (*FSBlob)(nil)
