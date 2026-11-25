package assets_test

// Integration test of asset publishing against a real S3-compatible service.
// Skipped unless GOSTACK_TEST_S3_ENDPOINT is set (same variables as
// modules/storage/s3_live_test.go: _BUCKET, _ACCESS_KEY, _SECRET_KEY, _REGION,
// _PATH_STYLE, _CREATE_BUCKET). scripts/drill/assets_minio.sh starts a
// throwaway MinIO and runs it. Objects live under a random game id.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/assets"
	"github.com/pillar/gostack/modules/storage"
)

// countingS3 records uploads to the real adapter.
type countingS3 struct {
	*storage.S3
	mu   sync.Mutex
	puts []string
}

func (c *countingS3) PutObject(ctx context.Context, key string, r io.Reader, size int64, o storage.PutOptions) error {
	c.mu.Lock()
	c.puts = append(c.puts, key)
	c.mu.Unlock()
	return c.S3.PutObject(ctx, key, r, size, o)
}

func (c *countingS3) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.puts
	c.puts = nil
	return p
}

func TestLiveAssetsS3(t *testing.T) {
	endpoint := os.Getenv("GOSTACK_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("GOSTACK_TEST_S3_ENDPOINT not set; live assets test skipped")
	}
	bucket, access, secret, region := os.Getenv("GOSTACK_TEST_S3_BUCKET"), os.Getenv("GOSTACK_TEST_S3_ACCESS_KEY"),
		os.Getenv("GOSTACK_TEST_S3_SECRET_KEY"), os.Getenv("GOSTACK_TEST_S3_REGION")
	if bucket == "" || access == "" || secret == "" || region == "" {
		t.Fatal("GOSTACK_TEST_S3_BUCKET, _ACCESS_KEY, _SECRET_KEY and _REGION are required with _ENDPOINT")
	}
	if os.Getenv("GOSTACK_TEST_S3_CREATE_BUCKET") == "1" {
		req, _ := http.NewRequest(http.MethodPut, strings.TrimRight(endpoint, "/")+"/"+bucket, nil)
		storage.Signer{AccessKeyID: access, SecretAccessKey: secret, Region: region, Service: "s3"}.Sign(req, storage.EmptySHA256, time.Now())
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 && res.StatusCode != 409 {
			t.Fatalf("create bucket: HTTP %d", res.StatusCode)
		}
	}
	s3, err := storage.NewS3(storage.S3Config{Endpoint: endpoint, Region: region, Bucket: bucket, AccessKeyID: access,
		SecretAccessKey: secret, PathStyle: os.Getenv("GOSTACK_TEST_S3_PATH_STYLE") != "0", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	st := &countingS3{S3: s3}
	o := sqlite.Options{Path: filepath.Join(t.TempDir(), "app.db"), ReadConnections: 2, Synchronous: "FULL",
		BusyTimeout: time.Second, WALAutocheckpoint: 1000, WriteQueue: 64}
	db := pgtest.Open(t, o)
	ctx := context.Background()
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	gid := "live-" + hex.EncodeToString(rb[:])
	if err := game.CreateGame(ctx, db, "default", gid, gid, nil); err != nil {
		t.Fatal(err)
	}
	m := &assets.Module{DB: db, Storage: st}
	build := func(files map[string]string) string {
		d := t.TempDir()
		for p, b := range files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755)
			if err := os.WriteFile(filepath.Join(d, p), []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB
	v1 := map[string]string{"index.html": "<html>v1</html>", "js/app.js": "v1()", "img/big.bin": big, "data/l.json": `{"l":1}`}
	r1, err := m.Publish(ctx, assets.PublishOptions{GameID: gid, Dir: build(v1), Label: "1"})
	if err != nil {
		t.Fatal(err)
	}
	keys1 := st.take()
	if len(keys1) != 5 || r1.Uploaded != 5 {
		t.Fatalf("publish 1: %+v %v", r1, keys1)
	}
	t.Logf("published %s: %d objects, %d bytes", r1.Release.ReleaseID, r1.Uploaded, r1.UploadedBytes)
	// The object store keeps the immutable Cache-Control and serves it (presigned GET).
	info, err := s3.Stat(ctx, keys1[0])
	if err != nil || info.CacheControl != assets.ImmutableCacheControl {
		t.Fatalf("stat %+v %v", info, err)
	}
	u, err := s3.Presign(ctx, http.MethodGet, r1.Release.EntryKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "<html>v1</html>" || res.Header.Get("Cache-Control") != assets.ImmutableCacheControl ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("entry GET %d %q %v", res.StatusCode, body, res.Header)
	}
	verify := func(id string) {
		t.Helper()
		rep, err := m.Verify(ctx, gid, id)
		if err != nil || !rep.OK() {
			t.Fatalf("verify %s: %+v %v", id, rep, err)
		}
		t.Logf("verify %s: %d objects, %d bytes, ok", rep.ReleaseID, rep.Checked, rep.Bytes)
	}
	verify("")

	v2 := map[string]string{"index.html": "<html>v2</html>", "js/app.js": "v2()", "img/big.bin": big, "data/l.json": `{"l":1}`}
	r2, err := m.Publish(ctx, assets.PublishOptions{GameID: gid, Dir: build(v2), Label: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if puts := st.take(); r2.Reused != 2 || len(puts) != 3 {
		t.Fatalf("publish 2 dedupe: %+v %v", r2, puts)
	}
	verify("")
	if prev, err := m.Rollback(ctx, gid, r1.Release.ReleaseID, "live"); err != nil || prev != r2.Release.ReleaseID {
		t.Fatalf("rollback %s %v", prev, err)
	}
	if cur, _ := m.Current(ctx, gid); cur.ReleaseID != r1.Release.ReleaseID {
		t.Fatal("rollback did not switch")
	}
	verify("")
	// Roll forward, prune v1: v1-only objects deleted from the bucket, v2 intact.
	if _, err := m.Rollback(ctx, gid, r2.Release.ReleaseID, "live"); err != nil {
		t.Fatal(err)
	}
	p, err := m.Prune(ctx, gid, 1, true, "live")
	if err != nil || p.Deleted != 3 || len(p.Failures) != 0 {
		t.Fatalf("prune %+v %v", p, err)
	}
	for _, k := range p.Delete {
		if _, err := s3.Stat(ctx, k); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("%s still present: %v", k, err)
		}
	}
	verify(r2.Release.ReleaseID)
	// Cleanup: delete the remaining release's objects (listed by its manifest).
	rc, _, err := s3.Get(ctx, r2.Release.ManifestKey)
	if err != nil {
		t.Fatal(err)
	}
	var man assets.Manifest
	err = json.NewDecoder(rc).Decode(&man)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{r2.Release.ManifestKey, r2.Release.EntryKey}
	for _, f := range man.Files {
		keys = append(keys, f.Key)
	}
	for _, k := range keys {
		if err := s3.Delete(ctx, k); err != nil {
			t.Errorf("cleanup %s: %v", k, err)
		}
	}
}
