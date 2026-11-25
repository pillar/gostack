package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newLocal(t *testing.T, publicURL string) (*Local, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	l, err := NewLocal(LocalConfig{Root: t.TempDir(), PublicURL: publicURL, Secret: bytes.Repeat([]byte("s"), 32),
		MaxObjectBytes: 1024, Timeout: time.Second, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return l, clk
}

func readAll(t *testing.T, s Storage, key string) (string, Info) {
	t.Helper()
	rc, info, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), info
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"a", "a/b-c_d.e", "users/42/avatar.png", strings.Repeat("x", MaxKeySegmentLen), strings.Repeat(strings.Repeat("x", 127)+"/", 4)[:MaxKeyLen-1] + "y"} {
		if err := ValidKey(k); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []string{"", "/a", "a/", "a//b", "..", "../x", "a/../b", "a/./b", ".", "a b", `a\b`, "ü", "a?b", "a%2e", "a\x00b", strings.Repeat("x", MaxKeyLen+1), strings.Repeat("x", MaxKeySegmentLen+1)} {
		if err := ValidKey(k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%q accepted", k)
		}
	}
}

func TestLocalPutGetDelete(t *testing.T) {
	l, _ := newLocal(t, "/files")
	ctx := context.Background()
	if err := l.Put(ctx, "a/b.txt", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	body, info := readAll(t, l, "a/b.txt")
	if body != "hello" || info.Size != 5 || info.ContentType != "text/plain" || info.ETag == "" {
		t.Fatalf("got %q %+v", body, info)
	}
	// Overwrite with unknown size; default content type.
	if err := l.Put(ctx, "a/b.txt", strings.NewReader("bye"), -1, ""); err != nil {
		t.Fatal(err)
	}
	if body, info := readAll(t, l, "a/b.txt"); body != "bye" || info.ContentType != "application/octet-stream" {
		t.Fatalf("overwrite: %q %+v", body, info)
	}
	// Keys differing only in case are distinct objects.
	if err := l.Put(ctx, "A/B.txt", strings.NewReader("upper"), 5, ""); err != nil {
		t.Fatal(err)
	}
	if body, _ := readAll(t, l, "a/b.txt"); body != "bye" {
		t.Fatalf("case alias: %q", body)
	}
	if err := l.Delete(ctx, "a/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Get(ctx, "a/b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := l.Delete(ctx, "a/b.txt"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if err := l.Put(ctx, "../x", strings.NewReader("x"), 1, ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("traversal: %v", err)
	}
	if err := l.Put(ctx, "x", strings.NewReader("x"), 1, "text/html\r\nX: y"); !errors.Is(err, ErrContentType) {
		t.Fatalf("content type: %v", err)
	}
}

func TestLocalLimitsAndAtomicity(t *testing.T) {
	l, _ := newLocal(t, "/files")
	ctx := context.Background()
	if err := l.Put(ctx, "k", strings.NewReader("orig"), 4, ""); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 2000)
	cases := []struct {
		r    io.Reader
		size int64
		want error
	}{
		{strings.NewReader(big), 2000, ErrTooLarge}, // declared too large
		{strings.NewReader(big), -1, ErrTooLarge},   // detected while streaming
		{strings.NewReader("short"), 10, ErrSizeMismatch},
		{strings.NewReader("longer than declared"), 3, ErrSizeMismatch},
	}
	for i, c := range cases {
		if err := l.Put(ctx, "k", c.r, c.size, ""); !errors.Is(err, c.want) {
			t.Errorf("case %d: %v, want %v", i, err, c.want)
		}
	}
	// Failed uploads leave the previous object intact and no temp files behind.
	if body, _ := readAll(t, l, "k"); body != "orig" {
		t.Fatalf("object changed by failed put: %q", body)
	}
	if ents, _ := os.ReadDir(filepath.Join(l.c.Root, "tmp")); len(ents) != 0 {
		t.Fatalf("temp files left: %v", ents)
	}
	// Exactly MaxObjectBytes is allowed.
	if err := l.Put(ctx, "max", strings.NewReader(big[:1024]), -1, ""); err != nil {
		t.Fatal(err)
	}
}

type slowReader struct{}

func (slowReader) Read(p []byte) (int, error) {
	time.Sleep(20 * time.Millisecond)
	p[0] = 'x'
	return 1, nil
}

func TestLocalTimeout(t *testing.T) {
	l, _ := newLocal(t, "/files")
	l.c.Timeout = 60 * time.Millisecond
	err := l.Put(context.Background(), "slow", io.LimitReader(slowReader{}, 100), 100, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestLocalPresignHandler(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	l, clk := newLocal(t, srv.URL+"/files")
	mux.Handle("/files/", l.Handler())
	ctx := context.Background()

	send := func(method, u, body string) (int, string, http.Header) {
		t.Helper()
		req, err := http.NewRequest(method, u, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "image/png")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	put, err := l.Presign(ctx, "PUT", "u/1/avatar.png", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	get, _ := l.Presign(ctx, "GET", "u/1/avatar.png", time.Minute)
	if !strings.HasPrefix(put, srv.URL+"/files/u/1/avatar.png?X-GS-Expires=") {
		t.Fatalf("url %s", put)
	}
	if st, b, _ := send("PUT", put, "PNGDATA"); st != 200 {
		t.Fatalf("put %d %s", st, b)
	}
	st, b, h := send("GET", get, "")
	if st != 200 || b != "PNGDATA" || h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("get %d %q %v", st, b, h)
	}
	// Signature binds method, key and expiry.
	if st, _, _ := send("PUT", get, "evil"); st != 403 {
		t.Fatalf("GET signature used for PUT: %d", st)
	}
	if st, _, _ := send("GET", strings.Replace(get, "avatar.png", "other.png", 1), ""); st != 403 {
		t.Fatalf("other key: %d", st)
	}
	tampered := get[:len(get)-1] + map[bool]string{true: "0", false: "1"}[get[len(get)-1] != '0']
	if st, _, _ := send("GET", tampered, ""); st != 403 {
		t.Fatalf("tampered signature: %d", st)
	}
	if st, _, _ := send("GET", strings.Replace(get, "X-GS-Expires=", "X-GS-Expires=1", 1), ""); st != 403 {
		t.Fatalf("changed expiry: %d", st)
	}
	if st, _, _ := send("DELETE", get, ""); st != 405 {
		t.Fatalf("delete: %d", st)
	}
	if st, _, _ := send("GET", srv.URL+"/files/a/%2e%2e/b?X-GS-Expires=1&X-GS-Signature=00", ""); st != 404 {
		t.Fatalf("traversal path: %d", st)
	}
	// Oversized presigned upload.
	big, _ := l.Presign(ctx, "PUT", "big", time.Minute)
	if st, _, _ := send("PUT", big, strings.Repeat("x", 2000)); st != 413 {
		t.Fatalf("oversized put: %d", st)
	}
	// Expiry.
	clk.Add(61 * time.Second)
	if st, _, _ := send("GET", get, ""); st != 403 {
		t.Fatalf("expired: %d", st)
	}
	if _, err := l.Presign(ctx, "DELETE", "k", time.Minute); err == nil {
		t.Fatal("presign DELETE accepted")
	}
	if _, err := l.Presign(ctx, "GET", "k", 8*24*time.Hour); err == nil {
		t.Fatal("presign ttl > 7d accepted")
	}
}

// PutObject records Cache-Control in the trailer; Stat reads metadata only;
// objects written by Put (no cache control) still read.
func TestLocalPutObjectCacheControlAndStat(t *testing.T) {
	l, _ := newLocal(t, "/v1/files")
	ctx := context.Background()
	cc := "public, max-age=31536000, immutable"
	if err := l.PutObject(ctx, "g/a.css", strings.NewReader("body{}"), 6, PutOptions{ContentType: "text/css", CacheControl: cc}); err != nil {
		t.Fatal(err)
	}
	info, err := l.Stat(ctx, "g/a.css")
	if err != nil || info.Size != 6 || info.CacheControl != cc || info.ContentType != "text/css" || info.ETag == "" {
		t.Fatalf("stat %+v %v", info, err)
	}
	if body, got := readAll(t, l, "g/a.css"); body != "body{}" || got.CacheControl != cc {
		t.Fatalf("get %q %+v", body, got)
	}
	if err := l.Put(ctx, "plain", strings.NewReader("x"), 1, ""); err != nil {
		t.Fatal(err)
	}
	if info, err := l.Stat(ctx, "plain"); err != nil || info.CacheControl != "" || info.Size != 1 {
		t.Fatalf("plain %+v %v", info, err)
	}
	if _, err := l.Stat(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := l.Stat(ctx, "../x"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid key: %v", err)
	}
	if err := l.PutObject(ctx, "k", strings.NewReader("x"), 1, PutOptions{CacheControl: strings.Repeat("a", 256)}); err == nil {
		t.Fatal("oversized cache control accepted")
	}
}
