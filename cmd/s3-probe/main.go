package main

// Command s3-probe validates an S3-compatible endpoint (RustFS, MinIO,
// cloud S3) against exactly the semantics mailez and mailezine rely on —
// bucket bootstrap, blob CRUD, error-code mapping, unknown-size multipart,
// recursive listing, and the conditional writes the HA lease store is
// built on (If-None-Match / If-Match + read-back confirm).
//
// Usage:
//
//	go run ./cmd/s3-probe -endpoint 127.0.0.1:9000 -access KEY -secret SECRET -bucket PROBE_BUCKET
import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	endpoint, accessKey, secretKey, bucket string
	useSSL                                 bool
	pass, fail                             int
)

func newClient() *minio.Client {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: "us-east-1",
	})
	if err != nil {
		panic(err)
	}
	return mc
}

func check(name string, err error, want string) {
	if err == nil {
		fmt.Printf("PASS  %-38s\n", name)
		pass++
		return
	}
	if want != "" && err.Error() == want {
		fmt.Printf("PASS  %-38s (expected: %s)\n", name, want)
		pass++
		return
	}
	fmt.Printf("FAIL  %-38s %v\n", name, err)
	fail++
}

func codeOf(err error) string {
	return minio.ToErrorResponse(err).Code
}

func main() {
	flag.StringVar(&endpoint, "endpoint", "127.0.0.1:9000", "S3-compatible endpoint host:port")
	flag.StringVar(&accessKey, "access", "", "access key")
	flag.StringVar(&secretKey, "secret", "", "secret key")
	flag.StringVar(&bucket, "bucket", "mailez-s3-probe", "probe bucket (created if absent)")
	flag.BoolVar(&useSSL, "ssl", false, "use TLS against the endpoint")
	flag.Parse()
	if accessKey == "" || secretKey == "" {
		fmt.Fprintln(os.Stderr, "s3-probe: -access and -secret are required")
		os.Exit(2)
	}
	ctx := context.Background()
	mc := newClient()

	// 1. Bucket bootstrap (drive/store.go newS3Store path).
	err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
	if err != nil {
		exists, e2 := mc.BucketExists(ctx, bucket)
		if !exists {
			fmt.Printf("FAIL  MakeBucket/BucketExists             %v / %v\n", err, e2)
			os.Exit(1)
		}
	}
	check("MakeBucket (idempotent)", nil, "")
	_, err = mc.BucketExists(ctx, bucket)
	check("BucketExists (HeadBucket)", err, "")

	// 2. Blob CRUD (drive + s3blob paths).
	body := bytes.NewReader([]byte("hello rustfs"))
	_, err = mc.PutObject(ctx, bucket, "probe/blob1", body, int64(body.Len()), minio.PutObjectOptions{ContentType: "text/plain"})
	check("PutObject (known size)", err, "")
	obj, err := mc.GetObject(ctx, bucket, "probe/blob1", minio.GetObjectOptions{})
	if err != nil {
		check("GetObject", err, "")
	} else {
		got, _ := io.ReadAll(obj)
		obj.Close()
		if string(got) != "hello rustfs" {
			check("GetObject content", errors.New("content mismatch: "+string(got)), "")
		} else {
			check("GetObject content", nil, "")
		}
	}
	info, err := mc.StatObject(ctx, bucket, "probe/blob1", minio.StatObjectOptions{})
	check("StatObject size", err, "")
	if err == nil && info.Size != 12 {
		check("StatObject size==12", fmt.Errorf("got %d", info.Size), "")
	}
	_, err = mc.StatObject(ctx, bucket, "probe/no-such-key", minio.StatObjectOptions{})
	if code := codeOf(err); code == "NoSuchKey" || code == "NotFound" {
		check("missing key error code = "+code, nil, "")
	} else {
		check("missing key error code", fmt.Errorf("got %q (mapS3Error needs NoSuchKey/NotFound)", code), "")
	}
	err = mc.RemoveObject(ctx, bucket, "probe/blob1", minio.RemoveObjectOptions{})
	check("RemoveObject", err, "")
	err = mc.RemoveObject(ctx, bucket, "probe/blob1", minio.RemoveObjectOptions{})
	check("RemoveObject idempotent", err, "")

	// 3. Unknown-size Put → minio-go multipart (s3blob.Put size<=0).
	rnd := make([]byte, 22<<20) // 22 MB > default 16 MB part? part size 16MiB → 2 parts
	_, _ = rand.Read(rnd)
	_, err = mc.PutObject(ctx, bucket, "probe/multipart", bytes.NewReader(rnd), -1, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	check("PutObject unknown size (multipart)", err, "")
	obj, err = mc.GetObject(ctx, bucket, "probe/multipart", minio.GetObjectOptions{})
	if err == nil {
		got, _ := io.ReadAll(obj)
		obj.Close()
		if !bytes.Equal(got, rnd) {
			check("multipart read-back bytes", fmt.Errorf("len got=%d want=%d", len(got), len(rnd)), "")
		} else {
			check("multipart read-back bytes", nil, "")
		}
	} else {
		check("multipart GetObject", err, "")
	}
	_ = mc.RemoveObject(ctx, bucket, "probe/multipart", minio.RemoveObjectOptions{})

	// 4. Recursive listing (backup.go ListObjects).
	for i := 0; i < 3; i++ {
		b := bytes.NewReader([]byte("x"))
		_, _ = mc.PutObject(ctx, bucket, fmt.Sprintf("mailez-backup/arch-%d.tar", i), b, 1, minio.PutObjectOptions{})
	}
	var names []string
	for o := range mc.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: "mailez-backup/", Recursive: true}) {
		if o.Err != nil {
			check("ListObjects", o.Err, "")
			break
		}
		names = append(names, o.Key)
	}
	if len(names) == 3 {
		check("ListObjects recursive (3 keys)", nil, "")
	} else {
		check("ListObjects recursive", fmt.Errorf("got %d keys: %v", len(names), names), "")
	}
	for _, n := range names {
		_ = mc.RemoveObject(ctx, bucket, n, minio.RemoveObjectOptions{})
	}

	// 5. Conditional writes — the HA lease store contract (ha.go put()).
	key := "probe/lease"
	_ = mc.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})

	// 5a. create-only: If-None-Match:* must succeed on absent object.
	claim := bytes.NewReader([]byte(`{"owner":"A","epoch":1}`))
	opts := minio.PutObjectOptions{ContentType: "application/json"}
	opts.SetMatchETagExcept("*")
	_, err = mc.PutObject(ctx, bucket, key, claim, int64(claim.Len()), opts)
	check("If-None-Match:* create (absent)", err, "")

	// 5b. create-only race: loser must get PreconditionFailed.
	opts2 := minio.PutObjectOptions{ContentType: "application/json"}
	opts2.SetMatchETagExcept("*")
	second := bytes.NewReader([]byte(`{"owner":"B","epoch":1}`))
	_, err = mc.PutObject(ctx, bucket, key, second, int64(second.Len()), opts2)
	if code := codeOf(err); code == "PreconditionFailed" {
		check("If-None-Match:* race → PreconditionFailed", nil, "")
	} else if err == nil {
		check("If-None-Match:* race", errors.New("second writer SUCCEEDED — conditional write ignored, HA lease unsafe"), "")
	} else {
		check("If-None-Match:* race", fmt.Errorf("unexpected code %q err %v", code, err), "")
	}
	// Read-back confirm: the object must still hold owner A.
	obj, _ = mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	got, _ := io.ReadAll(obj)
	obj.Close()
	if strings.Contains(string(got), `"owner":"A"`) {
		check("read-back confirms winner A", nil, "")
	} else {
		check("read-back confirms winner A", errors.New("object holds: "+string(got)), "")
	}

	// 5c. CAS renew: If-Match:<current etag> succeeds.
	cur, err := mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	check("StatObject for lease etag", err, "")
	etag := cur.ETag
	opts3 := minio.PutObjectOptions{ContentType: "application/json"}
	opts3.SetMatchETag(etag)
	renew := bytes.NewReader([]byte(`{"owner":"A","epoch":2}`))
	_, err = mc.PutObject(ctx, bucket, key, renew, int64(renew.Len()), opts3)
	check("If-Match:<etag> CAS renew", err, "")
	obj, _ = mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	got, _ = io.ReadAll(obj)
	obj.Close()
	if strings.Contains(string(got), `"epoch":2`) {
		check("CAS read-back epoch=2", nil, "")
	} else {
		check("CAS read-back epoch=2", errors.New("object holds: "+string(got)), "")
	}

	// 5d. Stale-etag CAS must fail PreconditionFailed.
	opts4 := minio.PutObjectOptions{ContentType: "application/json"}
	opts4.SetMatchETag(etag) // stale now
	stale := bytes.NewReader([]byte(`{"owner":"C","epoch":99}`))
	_, err = mc.PutObject(ctx, bucket, key, stale, int64(stale.Len()), opts4)
	if code := codeOf(err); code == "PreconditionFailed" {
		check("stale If-Match → PreconditionFailed", nil, "")
	} else if err == nil {
		check("stale If-Match", errors.New("stale write SUCCEEDED — CAS ignored, HA lease unsafe"), "")
	} else {
		check("stale If-Match", fmt.Errorf("unexpected code %q err %v", code, err), "")
	}
	obj, _ = mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	got, _ = io.ReadAll(obj)
	obj.Close()
	if strings.Contains(string(got), `"epoch":2`) {
		check("stale write did not land", nil, "")
	} else {
		check("stale write did not land", errors.New("object holds: "+string(got)), "")
	}

	// 6. Concurrent first-claim: 8 goroutines, exactly one winner.
	_ = mc.RemoveObject(ctx, bucket, "probe/race", minio.RemoveObjectOptions{})
	var mu sync.Mutex
	var wins, races int
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf(`{"owner":"w%d"}`, i))
			o := minio.PutObjectOptions{ContentType: "application/json"}
			o.SetMatchETagExcept("*")
			_, e := mc.PutObject(ctx, bucket, "probe/race", bytes.NewReader(payload), int64(len(payload)), o)
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				wins++
			} else if codeOf(e) == "PreconditionFailed" {
				races++
			}
		}(i)
	}
	wg.Wait()
	if wins == 1 && races == 7 {
		check("8-way first-claim: 1 win / 7 PrecondFailed", nil, "")
	} else {
		check("8-way first-claim", fmt.Errorf("wins=%d races=%d (want 1/7)", wins, races), "")
	}
	_ = mc.RemoveObject(ctx, bucket, "probe/race", minio.RemoveObjectOptions{})

	fmt.Printf("\n== probe done: %d pass, %d fail (bucket=%s endpoint=%s)\n", pass, fail, bucket, endpoint)
	if fail > 0 {
		os.Exit(1)
	}
}
