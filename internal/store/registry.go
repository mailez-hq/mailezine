// Backend opener registry. The built-in backends are Pebble KV + local FS
// blobs; add-on backend packages register their openers here from init, so
// OpenKVBlob can report a precise "backend not available" error for an
// unregistered name instead of a generic failure.
package store

// KVOpener opens a named KV backend. dsn/namespace carry the backend's
// connection parameters.
type KVOpener func(dsn, namespace string) (KV, error)

// S3BlobOpener opens an S3-compatible blob backend (MinIO or cloud S3);
// compressed selects gzip-at-rest.
type S3BlobOpener func(endpoint, accessKey, secretKey, bucket string, useSSL, compressed bool) (Blob, error)

var (
	kvOpeners    = map[string]KVOpener{}
	s3BlobOpener S3BlobOpener
)

// RegisterKVOpener installs the opener for one backend name; backend
// packages call it from init.
func RegisterKVOpener(name string, op KVOpener) { kvOpeners[name] = op }

// LookupKVOpener returns the opener registered for name, or nil.
func LookupKVOpener(name string) KVOpener { return kvOpeners[name] }

// SetS3BlobOpener installs the S3 blob opener (at most one).
func SetS3BlobOpener(op S3BlobOpener) { s3BlobOpener = op }

// S3BlobOpenerFor returns the registered S3 opener, or nil.
func S3BlobOpenerFor() S3BlobOpener { return s3BlobOpener }
