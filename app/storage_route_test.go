package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
)

// storageApp runs the real public server (httpServer: handler chain and
// listener timeouts) with local object storage.
func storageApp(t *testing.T, mut func(*config.Config)) (*App, string) {
	t.Helper()
	if pgtest.Enabled() {
		t.Skip("profile scale rejects local object storage")
	}
	c := testConfig(t)
	c.Storage.Driver = "local"
	c.Storage.Local.Root = filepath.Join(t.TempDir(), "objects")
	c.Storage.Local.PublicURL = "/v1/files"
	c.Storage.Local.PresignSecretRef = "env:GOSTACK_TEST_PRESIGN"
	t.Setenv("GOSTACK_TEST_PRESIGN", strings.Repeat("s", 32))
	c.Storage.MaxObjectBytes = 3 << 20
	if mut != nil {
		mut(&c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, c)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := a.httpServer(Hooks{})
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return a, "http://" + ln.Addr().String()
}

func presign(t *testing.T, a *App, method, key string) string {
	t.Helper()
	u, err := a.Mod.LocalStorage.Presign(context.Background(), method, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func send(t *testing.T, method, url string, body io.Reader, size int64) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	req.ContentLength = size
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, url, err)
	}
	return res, b
}

// Presigned transfers are bounded by storage.max_object_bytes, not by the
// generic http.max_body_bytes; other routes keep the generic limit.
func TestStorageRouteBypassesGenericBodyLimit(t *testing.T) {
	a, base := storageApp(t, func(c *config.Config) { c.HTTP.MaxBodyBytes = 2048 })
	obj := make([]byte, 2<<20)
	rand.Read(obj)
	res, b := send(t, "PUT", base+presign(t, a, "PUT", "big/two"), bytes.NewReader(obj), int64(len(obj)))
	if res.StatusCode != 200 {
		t.Fatalf("2 MiB presigned PUT: %d %s", res.StatusCode, b)
	}
	res, b = send(t, "GET", base+presign(t, a, "GET", "big/two"), nil, 0)
	if res.StatusCode != 200 || !bytes.Equal(b, obj) {
		t.Fatalf("presigned GET: %d, %d bytes", res.StatusCode, len(b))
	}
	if res.Header.Get("X-Request-Id") == "" {
		t.Fatal("storage route lost the common middleware (request id)")
	}
	over := make([]byte, 3<<20+1)
	if res, b := send(t, "PUT", base+presign(t, a, "PUT", "big/over"), bytes.NewReader(over), int64(len(over))); res.StatusCode != 413 {
		t.Fatalf("over max_object_bytes: %d %s", res.StatusCode, b)
	}
	// Unknown length (chunked) is cut off at max_object_bytes while streaming.
	if res, b := send(t, "PUT", base+presign(t, a, "PUT", "big/over"), io.MultiReader(bytes.NewReader(over)), -1); res.StatusCode != 413 {
		t.Fatalf("chunked over max_object_bytes: %d %s", res.StatusCode, b)
	}

	// Other routes are still limited by http.max_body_bytes (2 KiB here; the
	// save route's own limit is far larger).
	ctx := context.Background()
	if err := game.CreateGame(ctx, a.DB, "default", "demo", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := game.PublishConfig(ctx, a.DB, "demo", []byte(`{}`), "test", ""); err != nil {
		t.Fatal(err)
	}
	res, b = send(t, "POST", base+"/v1/games/demo/auth/guest", strings.NewReader("{}"), 2)
	if res.StatusCode != 201 {
		t.Fatalf("guest: %d %s", res.StatusCode, b)
	}
	tok := string(b[strings.Index(string(b), `"access_token":"`)+16:])
	tok = tok[:strings.IndexByte(tok, '"')]
	save := `{"expected_revision":0,"payload":{"x":"` + strings.Repeat("a", 3000) + `"}}`
	req, _ := http.NewRequest("PUT", base+"/v1/games/demo/saves/main", strings.NewReader(save))
	req.Header.Set("Authorization", "Bearer "+tok)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 413 {
		t.Fatalf("generic body limit not applied to other routes: %d", r2.StatusCode)
	}
}

type slowReader struct {
	r     io.Reader
	chunk int
	every time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.every)
	return s.r.Read(p[:min(len(p), s.chunk)])
}

// A transfer longer than http.request_timeout (+5 s listener slack) completes:
// the storage route uses storage.timeout for its context and read/write deadlines.
func TestStorageRouteSlowTransferOutlastsRequestTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~6 s")
	}
	a, base := storageApp(t, func(c *config.Config) { c.HTTP.RequestTimeout = 200 * time.Millisecond })
	obj := make([]byte, 3<<20)
	rand.Read(obj)
	if res, b := send(t, "PUT", base+presign(t, a, "PUT", "fast/obj"), bytes.NewReader(obj), int64(len(obj))); res.StatusCode != 200 {
		t.Fatalf("fast PUT: %d %s", res.StatusCode, b)
	}
	putURL, getURL := base+presign(t, a, "PUT", "slow/obj"), base+presign(t, a, "GET", "fast/obj")
	errs := make(chan error, 2)
	go func() { // upload: 3 MiB in 64 chunks every 100 ms, ~6.4 s > 200 ms + 5 s
		req, _ := http.NewRequest("PUT", putURL, &slowReader{r: bytes.NewReader(obj), chunk: len(obj) / 64, every: 100 * time.Millisecond})
		req.ContentLength = int64(len(obj))
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode != 200 {
				err = fmt.Errorf("status %d", res.StatusCode)
			}
		}
		errs <- wrapErr("slow PUT", err)
	}()
	go func() { // download read in 32 KiB pieces every 100 ms, ~9.6 s
		res, err := http.Get(getURL)
		if err == nil {
			defer res.Body.Close()
			var got []byte
			if got, err = io.ReadAll(&slowReader{r: res.Body, chunk: 32 << 10, every: 100 * time.Millisecond}); err == nil && !bytes.Equal(got, obj) {
				err = fmt.Errorf("got %d bytes, content differs", len(got))
			}
		}
		errs <- wrapErr("slow GET", err)
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func wrapErr(what string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}
