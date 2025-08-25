// Blob is the immutable-byte object store of mailezine. Implementations:
// MinIO (S3), local filesystem (default for small deployments), MemoryBlob.
package store

import (
	"context"
	"io"
)

// Blob stores immutable content-addressed objects (message bodies,
// attachments). Idempotent Put: writing the same id twice is not an error.
type Blob interface {
	// Put stores r under id. size is the exact byte count when known
	// (>=0); implementations may use it to pick a single-request upload
	// path (S3: avoids multipart). Pass -1 for unknown size.
	Put(ctx context.Context, id string, size int64, r io.Reader) (int64, error)
	Get(ctx context.Context, id string, w io.Writer) error
	Delete(ctx context.Context, id string) error
	Stat(ctx context.Context, id string) (int64, error)
}
