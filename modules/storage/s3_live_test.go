package storage

// Integration test of the S3 adapter against a real S3-compatible service
// (MinIO, R2, AWS). It is skipped unless GOSTACK_TEST_S3_ENDPOINT is set:
//
//	GOSTACK_TEST_S3_ENDPOINT=http://127.0.0.1:9000  (scheme://host[:port])
//	GOSTACK_TEST_S3_BUCKET=gostack-it
//	GOSTACK_TEST_S3_ACCESS_KEY=...  GOSTACK_TEST_S3_SECRET_KEY=...
//	GOSTACK_TEST_S3_REGION=us-east-1 (R2: auto)
//	GOSTACK_TEST_S3_PATH_STYLE=1        (default 1; 0 = virtual-host style)
//	GOSTACK_TEST_S3_CREATE_BUCKET=1     (create the bucket first; MinIO only)
//
// scripts/drill/s3_minio.sh starts a throwaway MinIO and runs this test.
// Every object lives under a random prefix and is deleted at the end.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type liveS3Env struct {
	endpoint, bucket, access, secret, region string
	pathStyle                                bool
}

func liveS3(t *testing.T) liveS3Env {
	t.Helper()
	e := liveS3Env{
		endpoint:  os.Getenv("GOSTACK_TEST_S3_ENDPOINT"),
		bucket:    os.Getenv("GOSTACK_TEST_S3_BUCKET"),
		access:    os.Getenv("GOSTACK_TEST_S3_ACCESS_KEY"),
		secret:    os.Getenv("GOSTACK_TEST_S3_SECRET_KEY"),
		region:    os.Getenv("GOSTACK_TEST_S3_REGION"),
		pathStyle: os.Getenv("GOSTACK_TEST_S3_PATH_STYLE") != "0",
	}
	if e.endpoint == "" {
		t.Skip("GOSTACK_TEST_S3_ENDPOINT not set; live S3 test skipped")
	}
	if e.bucket == "" || e.access == "" || e.secret == "" || e.region == "" {
		t.Fatal("GOSTACK_TEST_S3_BUCKET, _ACCESS_KEY, _SECRET_KEY and _REGION are required with _ENDPOINT")
	}
	return e
}

func (e liveS3Env) cfg() S3Config {
	return S3Config{Endpoint: e.endpoint, Region: e.region, Bucket: e.bucket, AccessKeyID: e.access,
		SecretAccessKey: e.secret, PathStyle: e.pathStyle, Timeout: 30 * time.Second, TempDir: os.TempDir()}
}

func liveAdapter(t *testing.T, cfg S3Config) *S3 {
	t.Helper()
	s, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// createLiveBucket issues a signed PUT /bucket (path style). 409 (already
// owned) counts as success.
func createLiveBucket(t *testing.T, e liveS3Env) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, strings.TrimRight(e.endpoint, "/")+"/"+e.bucket, nil)
	Signer{AccessKeyID: e.access, SecretAccessKey: e.secret, Region: e.region, Service: "s3"}.Sign(req, EmptySHA256, time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 409 {
		t.Fatalf("create bucket: HTTP %d %s", resp.StatusCode, b)
	}
}

func liveGet(t *testing.T, s *S3, key string) ([]byte, Info) {
	t.Helper()
	rc, info, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("Get %s body: %v", key, err)
	}
	return b, info
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// onlyReader hides Len/Seek so the adapter cannot learn the size.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func TestLiveS3(t *testing.T) {
	e := liveS3(t)
	ctx := context.Background()
	if os.Getenv("GOSTACK_TEST_S3_CREATE_BUCKET") == "1" {
		createLiveBucket(t, e)
	}
	prefix := "gostack-it/" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(randBytes(4)) + "/"
	var created []string
	s := liveAdapter(t, e.cfg())
	t.Cleanup(func() {
		for _, k := range created {
			if err := s.Delete(ctx, k); err != nil {
				t.Errorf("cleanup delete %s: %v", k, err)
			}
		}
	})
	put := func(t *testing.T, s *S3, key string, body []byte, size int64, ct string) {
		t.Helper()
		created = append(created, key)
		var r io.Reader = bytes.NewReader(body)
		if size < 0 {
			r = onlyReader{r}
		}
		if err := s.Put(ctx, key, r, size, ct); err != nil {
			t.Fatalf("Put %s (size %d): %v", key, size, err)
		}
	}
	mustMissing := func(t *testing.T, s *S3, key string) {
		t.Helper()
		if _, _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get %s: want ErrNotFound, got %v", key, err)
		}
	}

	t.Run("PutGetDeleteUnsignedPayload", func(t *testing.T) {
		key, body := prefix+"unsigned.bin", randBytes(256<<10)
		put(t, s, key, body, int64(len(body)), "application/x-test")
		got, info := liveGet(t, s, key)
		if !bytes.Equal(got, body) || info.Size != int64(len(body)) || info.ContentType != "application/x-test" || info.ETag == "" {
			t.Fatalf("round trip mismatch: len=%d info=%+v", len(got), info)
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		mustMissing(t, s, key)
		if err := s.Delete(ctx, key); err != nil {
			t.Fatalf("delete of missing key: %v", err)
		}
	})

	t.Run("SignedPayload", func(t *testing.T) {
		cfg := e.cfg()
		cfg.SignPayload = true
		ss := liveAdapter(t, cfg)
		key, body := prefix+"signed.bin", randBytes(300<<10)
		put(t, ss, key, body, int64(len(body)), "")
		got, info := liveGet(t, ss, key)
		if !bytes.Equal(got, body) || info.ContentType != "application/octet-stream" {
			t.Fatalf("signed payload round trip mismatch: len=%d info=%+v", len(got), info)
		}
		// Empty object, signed.
		put(t, ss, prefix+"empty", nil, 0, "text/plain")
		if got, info := liveGet(t, ss, prefix+"empty"); len(got) != 0 || info.Size != 0 {
			t.Fatalf("empty object: len=%d info=%+v", len(got), info)
		}
	})

	t.Run("UnknownSizeStreaming", func(t *testing.T) {
		key, body := prefix+"stream.bin", randBytes(3<<20+17)
		put(t, s, key, body, -1, "application/octet-stream")
		if got, info := liveGet(t, s, key); !bytes.Equal(got, body) || info.Size != int64(len(body)) {
			t.Fatalf("unknown-size round trip mismatch: len=%d info=%+v", len(got), info)
		}
	})

	t.Run("SizeLimits", func(t *testing.T) {
		cfg := e.cfg()
		cfg.MaxObjectBytes = 1024
		ls := liveAdapter(t, cfg)
		big := randBytes(2048)
		key := prefix + "too-large"
		created = append(created, key)
		if err := ls.Put(ctx, key, bytes.NewReader(big), int64(len(big)), ""); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("declared size over limit: %v", err)
		}
		if err := ls.Put(ctx, key, onlyReader{bytes.NewReader(big)}, -1, ""); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("unknown size over limit: %v", err)
		}
		if err := ls.Put(ctx, key, bytes.NewReader(big[:100]), 200, ""); !errors.Is(err, ErrSizeMismatch) {
			t.Fatalf("short body: %v", err)
		}
		if err := ls.Put(ctx, key, bytes.NewReader(big[:300]), 200, ""); !errors.Is(err, ErrSizeMismatch) {
			t.Fatalf("long body: %v", err)
		}
		mustMissing(t, ls, key)
		put(t, ls, key, big[:1024], 1024, "")
		if got, _ := liveGet(t, ls, key); !bytes.Equal(got, big[:1024]) {
			t.Fatal("object at exactly the limit not stored intact")
		}
	})

	t.Run("SpecialKeys", func(t *testing.T) {
		long := prefix + strings.Repeat("k", MaxKeyLen-len(prefix))
		keys := []string{
			prefix + "UPPER.lower_09-x",
			prefix + "a/b/c/d/e.json",
			prefix + "...dots/a..b/-_-/.hidden",
			prefix + "~nope", // rejected below
			long,
		}
		for _, k := range keys {
			if ValidKey(k) != nil {
				if err := s.Put(ctx, k, strings.NewReader("x"), 1, ""); !errors.Is(err, ErrInvalidKey) {
					t.Fatalf("invalid key %q accepted: %v", k, err)
				}
				continue
			}
			body := []byte("key=" + k)
			put(t, s, k, body, int64(len(body)), "text/plain")
			if got, _ := liveGet(t, s, k); !bytes.Equal(got, body) {
				t.Fatalf("key %q round trip mismatch", k)
			}
			u, err := s.Presign(ctx, http.MethodGet, k, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Get(u)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !bytes.Equal(got, body) {
				t.Fatalf("presigned GET of %q: HTTP %d", k, resp.StatusCode)
			}
		}
	})

	t.Run("PresignedGetAndPut", func(t *testing.T) {
		key, body := prefix+"presigned.txt", []byte("uploaded through a presigned PUT URL")
		created = append(created, key)
		pu, err := s.Presign(ctx, http.MethodPut, key, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPut, pu, bytes.NewReader(body))
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("presigned PUT: HTTP %d %s", resp.StatusCode, msg)
		}
		if got, _ := liveGet(t, s, key); !bytes.Equal(got, body) {
			t.Fatal("presigned PUT body mismatch")
		}
		gu, err := s.Presign(ctx, http.MethodGet, key, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		resp, err = http.Get(gu)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(got, body) {
			t.Fatalf("presigned GET: HTTP %d %q", resp.StatusCode, got)
		}
		// A GET URL must not authorize PUT, and a tampered signature fails.
		req, _ = http.NewRequest(http.MethodPut, gu, strings.NewReader("evil"))
		if code := liveStatus(t, req); code != 403 {
			t.Fatalf("PUT with presigned GET URL: HTTP %d, want 403", code)
		}
		tampered := gu[:len(gu)-1] + map[bool]string{true: "0", false: "1"}[gu[len(gu)-1] != '0']
		req, _ = http.NewRequest(http.MethodGet, tampered, nil)
		if code := liveStatus(t, req); code != 403 {
			t.Fatalf("tampered signature: HTTP %d, want 403", code)
		}
	})

	t.Run("PresignExpired", func(t *testing.T) {
		key, body := prefix+"expiry.txt", []byte("expiry")
		put(t, s, key, body, int64(len(body)), "")
		cfg := e.cfg()
		cfg.Now = func() time.Time { return time.Now().Add(-10 * time.Minute) }
		old := liveAdapter(t, cfg)
		u, err := old.Presign(ctx, http.MethodGet, key, time.Minute) // expired 9 minutes ago
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if code := liveStatus(t, req); code != 403 {
			t.Fatalf("expired presigned GET: HTTP %d, want 403", code)
		}
		short := liveAdapter(t, e.cfg())
		u, err = short.Presign(ctx, http.MethodGet, key, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2500 * time.Millisecond)
		req, _ = http.NewRequest(http.MethodGet, u, nil)
		if code := liveStatus(t, req); code != 403 {
			t.Fatalf("1s presigned GET after 2.5s: HTTP %d, want 403", code)
		}
	})

	t.Run("WrongCredentials", func(t *testing.T) {
		badSecret := "wrong-" + hex.EncodeToString(randBytes(12))
		cfg := e.cfg()
		cfg.SecretAccessKey = badSecret
		bad := liveAdapter(t, cfg)
		key := prefix + "denied"
		created = append(created, key)
		checks := map[string]error{}
		checks["put"] = bad.Put(ctx, key, strings.NewReader("x"), 1, "")
		_, _, checks["get"] = bad.Get(ctx, prefix+"expiry.txt")
		checks["delete"] = bad.Delete(ctx, prefix+"expiry.txt")
		cfg.SignPayload = true
		checks["put-signed"] = liveAdapter(t, cfg).Put(ctx, key, strings.NewReader("x"), 1, "")
		for op, err := range checks {
			var se *s3Error
			if !errors.As(err, &se) || se.Status != 403 {
				t.Fatalf("%s with wrong secret: want *s3Error 403, got %v", op, err)
			}
			if msg := err.Error(); strings.Contains(msg, badSecret) || strings.Contains(msg, e.secret) {
				t.Fatalf("%s error leaks a secret", op)
			}
			t.Logf("%s with wrong secret -> %v", op, err)
		}
		mustMissing(t, s, key)
		if got, _ := liveGet(t, s, prefix+"expiry.txt"); string(got) != "expiry" {
			t.Fatal("object did not survive the unauthorized delete")
		}
		u, _ := bad.Presign(ctx, http.MethodGet, prefix+"expiry.txt", time.Minute)
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if code := liveStatus(t, req); code != 403 {
			t.Fatalf("presigned GET with wrong secret: HTTP %d, want 403", code)
		}
	})

	t.Run("MissingObject", func(t *testing.T) {
		mustMissing(t, s, prefix+"never-written")
	})
}

func liveStatus(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}
