// Package s3test provides a minimal in-process S3-compatible server used by
// tests that exercise real S3/MinIO-backed paths (Blob contract, migrate).
// It is a plain package so any test can import it; production binaries never
// reference it, so it is eliminated at link time.
package s3test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server is a minimal S3-compatible server sufficient for the Blob contract:
// single-bucket PUT (single and multipart)/GET/HEAD/DELETE plus the
// bucket-location probe. minio-go streams readers of unknown size through
// multipart upload, so that flow is part of the contract.
type Server struct {
	mu      sync.Mutex
	m       map[string][]byte
	uploads map[string]map[int][]byte
	seq     int
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) < 2 || parts[1] == "" {
		// Bucket-level request (e.g. location probe).
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`))
		return
	}
	key := parts[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		if partNo := q.Get("partNumber"); partNo != "" {
			uploadID := q.Get("uploadId")
			body, _ := io.ReadAll(r.Body)
			body = decodeAWSChunked(r, body)
			n, _ := strconv.Atoi(partNo)
			s.uploads[uploadID][n] = body
			w.Header().Set("ETag", `"part-`+partNo+`"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		body = decodeAWSChunked(r, body)
		s.m[key] = body
		w.Header().Set("ETag", `"etag-`+key+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodPost:
		if q.Has("uploads") {
			// InitiateMultipartUpload.
			s.seq++
			uploadID := "u" + strconv.Itoa(s.seq)
			s.uploads[uploadID] = map[int][]byte{}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, parts[0], key, uploadID)
			return
		}
		if uploadID := q.Get("uploadId"); uploadID != "" {
			// CompleteMultipartUpload: concatenate parts in order.
			partsMap := s.uploads[uploadID]
			nums := make([]int, 0, len(partsMap))
			for n := range partsMap {
				nums = append(nums, n)
			}
			sort.Ints(nums)
			var out []byte
			for _, n := range nums {
				out = append(out, partsMap[n]...)
			}
			delete(s.uploads, uploadID)
			s.m[key] = out
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<CompleteMultipartUploadResult><Location>http://%s/%s</Location><Bucket>%s</Bucket><Key>%s</Key><ETag>"etag"</ETag></CompleteMultipartUploadResult>`, r.Host, key, parts[0], key)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case http.MethodGet:
		v, ok := s.m[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>not found</Message></Error>`))
			return
		}
		w.Header().Set("ETag", `"etag-`+key+`"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(v)
	case http.MethodHead:
		v, ok := s.m[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(v)))
		w.Header().Set("ETag", `"etag-`+key+`"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(s.m, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// New starts an in-process S3-compatible server. It returns the endpoint
// (host:port), a getter for the current object map, and a close func.
func New(t *testing.T) (endpoint string, objects func() map[string][]byte, cleanup func()) {
	t.Helper()
	s := &Server{m: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(s)
	objects = func() map[string][]byte {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := make(map[string][]byte, len(s.m))
		for k, v := range s.m {
			out[k] = append([]byte(nil), v...)
		}
		return out
	}
	return strings.TrimPrefix(srv.URL, "http://"), objects, srv.Close
}

// decodeAWSChunked decodes the STREAMING-AWS4-HMAC-SHA256-PAYLOAD framing
// minio-go uses for readers of unknown size. MinIO performs this decoding
// server-side; the mock must do the same to store the real payload.
func decodeAWSChunked(r *http.Request, body []byte) []byte {
	if !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return body
	}
	var out []byte
	i := 0
	for i < len(body) {
		j := bytes.Index(body[i:], []byte("\r\n"))
		if j < 0 {
			break
		}
		line := body[i : i+j]
		i += j + 2
		sizeHex := line
		if k := bytes.IndexByte(line, ';'); k >= 0 {
			sizeHex = line[:k]
		}
		n, err := strconv.ParseInt(string(sizeHex), 16, 64)
		if err != nil || n <= 0 {
			break // zero-size chunk terminates the stream
		}
		if i+int(n)+2 > len(body) {
			break
		}
		out = append(out, body[i:i+int(n)]...)
		i += int(n)
		if body[i] == '\r' && body[i+1] == '\n' {
			i += 2
		}
	}
	return out
}
