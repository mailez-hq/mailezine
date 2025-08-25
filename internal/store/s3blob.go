// S3Blob stores immutable blobs in an S3-compatible object store (MinIO or
// any cloud S3). Put streams with an unknown size; Stat/Get map missing
// objects to ErrNotFound. Delete is idempotent, matching S3 semantics.
package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Blob implements Blob over an S3-compatible endpoint.
type S3Blob struct {
	mc         *minio.Client
	bucket     string
	compressed bool
}

// NewS3Blob builds a client. Region is pinned so the client skips the
// bucket-location round trip (ARCHITECTURE.md §3.4: MinIO/cloud S3
// interchangeable).
func NewS3Blob(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*S3Blob, error) {
	return newS3Blob(endpoint, accessKey, secretKey, bucket, useSSL, false)
}

// NewS3BlobCompressed stores blobs gzip-compressed at rest.
func NewS3BlobCompressed(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*S3Blob, error) {
	return newS3Blob(endpoint, accessKey, secretKey, bucket, useSSL, true)
}

func newS3Blob(endpoint, accessKey, secretKey, bucket string, useSSL, compressed bool) (*S3Blob, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: "us-east-1",
	})
	if err != nil {
		return nil, err
	}
	return &S3Blob{mc: mc, bucket: bucket, compressed: compressed}, nil
}

// EnsureBucket creates the bucket when it does not exist yet.
func (b *S3Blob) EnsureBucket(ctx context.Context) error {
	exists, err := b.mc.BucketExists(ctx, b.bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return b.mc.MakeBucket(ctx, b.bucket, minio.MakeBucketOptions{})
}

func (b *S3Blob) Put(ctx context.Context, id string, size int64, r io.Reader) (int64, error) {
	if err := validateBlobID(id); err != nil {
		return 0, err
	}
	logicalSize := size
	if b.compressed {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := io.Copy(gz, r); err != nil {
			return 0, err
		}
		if err := gz.Close(); err != nil {
			return 0, err
		}
		r = &buf
		size = int64(buf.Len())
	}
	if size <= 0 {
		size = -1 // unknown: minio-go falls back to multipart upload
	}
	_, err := b.mc.PutObject(ctx, b.bucket, id, r, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return 0, err
	}
	return logicalSize, nil
}

func (b *S3Blob) Get(ctx context.Context, id string, w io.Writer) error {
	obj, err := b.mc.GetObject(ctx, b.bucket, id, minio.GetObjectOptions{})
	if err != nil {
		return mapS3Error(err)
	}
	defer obj.Close()
	if _, err := obj.Stat(); err != nil {
		return mapS3Error(err)
	}
	if b.compressed {
		gz, err := gzip.NewReader(obj)
		if err == nil {
			defer gz.Close()
			_, err = io.Copy(w, gz)
			return err
		}
		// Not gzip: legacy uncompressed blob.
		if _, err := obj.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	_, err = io.Copy(w, obj)
	return err
}

func (b *S3Blob) Delete(ctx context.Context, id string) error {
	return b.mc.RemoveObject(ctx, b.bucket, id, minio.RemoveObjectOptions{})
}

func (b *S3Blob) Stat(ctx context.Context, id string) (int64, error) {
	info, err := b.mc.StatObject(ctx, b.bucket, id, minio.StatObjectOptions{})
	if err != nil {
		return 0, mapS3Error(err)
	}
	return info.Size, nil
}

func mapS3Error(err error) error {
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchKey", "NotFound":
		return ErrNotFound
	default:
		return err
	}
}

var _ Blob = (*S3Blob)(nil)
