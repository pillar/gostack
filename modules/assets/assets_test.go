package assets_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/assets"
	"github.com/pillar/gostack/modules/storage"
)

// faultStore wraps the local adapter, records uploads and can fail or block them.
type faultStore struct {
	*storage.Local
	mu      sync.Mutex
	puts    []string
	failOn  func(key string) error
	block   chan struct{} // uploads wait until closed
	entered chan struct{} // signalled (non-blocking) when an upload starts
}

func (f *faultStore) PutObject(ctx context.Context, key string, r io.Reader, size int64, o storage.PutOptions) error {
	f.mu.Lock()
	fail, blk, ent := f.failOn, f.block, f.entered
	f.mu.Unlock()
	if ent != nil {
		select {
		case ent <- struct{}{}:
		default:
		}
	}
	if blk != nil {
		<-blk
	}
	if fail != nil {
		if err := fail(key); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.puts = append(f.puts, key)
	f.mu.Unlock()
	return f.Local.PutObject(ctx, key, r, size, o)
}

func (f *faultStore) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	return f.PutObject(ctx, key, r, size, storage.PutOptions{ContentType: ct})
}

func (f *faultStore) takePuts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.puts
	f.puts = nil
	return p
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type fixture struct {
	t   *testing.T
	m   *assets.Module
	st  *faultStore
	clk *clock
	ctx context.Context
}

func setup(t *testing.T) *fixture {
	t.Helper()
	o := sqlite.Options{Path: filepath.Join(t.TempDir(), "app.db"), ReadConnections: 2, Synchronous: "FULL",
		BusyTimeout: time.Second, WALAutocheckpoint: 1000, WriteQueue: 64}
	db := pgtest.Open(t, o)
	ctx := context.Background()
	for _, g := range []string{"alpha", "beta"} {
		if err := game.CreateGame(ctx, db, "default", g, g, nil); err != nil {
			t.Fatal(err)
		}
	}
	l, err := storage.NewLocal(storage.LocalConfig{Root: t.TempDir(), PublicURL: "/v1/files", Secret: bytes.Repeat([]byte("k"), 32)})
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	st := &faultStore{Local: l}
	return &fixture{t: t, m: &assets.Module{DB: db, Storage: st, Now: clk.Now}, st: st, clk: clk, ctx: ctx}
}

// build writes files (path -> body) into a new directory.
func (f *fixture) build(files map[string]string) string {
	f.t.Helper()
	dir := f.t.TempDir()
	for p, body := range files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir
}

func (f *fixture) publish(dir, label string) assets.PublishResult {
	f.t.Helper()
	f.clk.Add(time.Second) // distinct release ids / created_at
	res, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: dir, Label: label})
	if err != nil {
		f.t.Fatalf("publish %s: %v", label, err)
	}
	return res
}

func (f *fixture) current() string {
	f.t.Helper()
	r, err := f.m.Current(f.ctx, "alpha")
	if err != nil {
		f.t.Fatal(err)
	}
	return r.ReleaseID
}

func (f *fixture) verifyOK(release string) assets.VerifyReport {
	f.t.Helper()
	rep, err := f.m.Verify(f.ctx, "alpha", release)
	if err != nil {
		f.t.Fatal(err)
	}
	if !rep.OK() {
		f.t.Fatalf("verify %s: %+v", release, rep.Problems)
	}
	return rep
}

func (f *fixture) exists(key string) bool {
	f.t.Helper()
	_, err := f.st.Stat(f.ctx, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		f.t.Fatal(err)
	}
	return err == nil
}

func httpCode(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

var v1Files = map[string]string{
	"index.html":      "<html>v1</html>",
	"js/app.js":       "console.log('v1')",
	"img/logo.png":    "PNGDATA",
	"data/level.json": `{"level":1}`,
}

// G-A07: publish -> verify -> second publish deduplicates unchanged files ->
// rollback serves the old release and verifies -> prune keeps every object the
// kept and current releases reference.
func TestPublishDedupeRollbackPrune(t *testing.T) {
	f := setup(t)
	r1 := f.publish(f.build(v1Files), "1.0.0")
	if r1.Previous != "" || r1.Uploaded != 5 || r1.Reused != 0 || r1.Release.FileCount != 4 {
		t.Fatalf("first publish %+v", r1)
	}
	// Order: assets, then manifest, then entry (the pointer switches last, in the DB).
	puts := f.st.takePuts()
	if len(puts) != 5 || !strings.Contains(puts[3], "/manifests/") || !strings.Contains(puts[4], "/releases/") {
		t.Fatalf("upload order %v", puts)
	}
	for _, k := range puts {
		info, err := f.st.Stat(f.ctx, k)
		if err != nil || info.CacheControl != assets.ImmutableCacheControl {
			t.Fatalf("%s: %+v %v", k, info, err)
		}
	}
	id1 := r1.Release.ReleaseID
	if f.current() != id1 || !assets.ValidReleaseID(id1) {
		t.Fatalf("current %s, want %s", f.current(), id1)
	}
	if rep := f.verifyOK(""); rep.Checked != 5 || rep.ReleaseID != id1 {
		t.Fatalf("verify %+v", rep)
	}

	v2 := map[string]string{"index.html": "<html>v2</html>", "js/app.js": "console.log('v2')", "img/logo.png": "PNGDATA",
		"data/level.json": `{"level":1}`, "data/level2.json": `{"level":2}`}
	r2 := f.publish(f.build(v2), "1.1.0")
	puts = f.st.takePuts()
	// Unchanged logo.png and level.json are not re-uploaded.
	if r2.Previous != id1 || r2.Reused != 2 || r2.Uploaded != 4 || len(puts) != 4 {
		t.Fatalf("second publish %+v puts %v", r2, puts)
	}
	for _, k := range puts {
		if strings.Contains(k, "logo.png") || strings.HasSuffix(k, "/level.json") {
			t.Fatalf("unchanged file re-uploaded: %s", k)
		}
	}
	id2 := r2.Release.ReleaseID
	f.verifyOK(id2)

	// Rollback to v1: pointer only, old objects still present.
	prev, err := f.m.Rollback(f.ctx, "alpha", id1, "test")
	if err != nil || prev != id2 || f.current() != id1 {
		t.Fatalf("rollback prev=%s err=%v current=%s", prev, err, f.current())
	}
	f.verifyOK("")
	if prev, err := f.m.Rollback(f.ctx, "alpha", id1, "test"); err != nil || prev != id1 {
		t.Fatalf("repeated rollback prev=%s err=%v", prev, err)
	}

	// A third release; v1 stays current via rollback afterwards.
	r3 := f.publish(f.build(map[string]string{"index.html": "<html>v3</html>", "js/app.js": "console.log('v3')", "img/logo.png": "PNGDATA"}), "1.2.0")
	id3 := r3.Release.ReleaseID
	if _, err := f.m.Rollback(f.ctx, "alpha", id1, "test"); err != nil {
		t.Fatal(err)
	}
	rels, cur, err := f.m.List(f.ctx, "alpha")
	if err != nil || cur != id1 || len(rels) != 3 || rels[0].ReleaseID != id3 || !rels[2].Current {
		t.Fatalf("list cur=%s %+v %v", cur, rels, err)
	}

	// Dry run deletes nothing.
	before := 0
	r2objs := []string{r2.Release.ManifestKey, r2.Release.EntryKey}
	plan, err := f.m.Prune(f.ctx, "alpha", 1, false, "test")
	if err != nil || plan.Applied || len(plan.Prune) != 1 || plan.Prune[0] != id2 || len(plan.Keep) != 2 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	for _, k := range plan.Delete {
		if f.exists(k) {
			before++
		}
	}
	if before != len(plan.Delete) || len(plan.Delete) != 4 { // v2 manifest, entry, app.js v2, level2.json
		t.Fatalf("dry run: %d of %d present, keys %v", before, len(plan.Delete), plan.Delete)
	}
	for _, k := range r2objs {
		if !contains(plan.Delete, k) {
			t.Fatalf("plan misses %s", k)
		}
	}
	// keep=1 keeps the newest (v3) and the current (v1); prunes v2.
	plan, err = f.m.Prune(f.ctx, "alpha", 1, true, "test")
	if err != nil || !plan.Applied || plan.Deleted != 4 || len(plan.Failures) != 0 {
		t.Fatalf("prune %+v %v", plan, err)
	}
	for _, k := range plan.Delete {
		if f.exists(k) {
			t.Fatalf("%s not deleted", k)
		}
	}
	f.verifyOK(id1)
	f.verifyOK(id3)
	if _, err := f.m.Rollback(f.ctx, "alpha", id2, "test"); httpCode(err) != "release_pruned" {
		t.Fatalf("rollback to pruned: %v", err)
	}
	if _, err := f.m.Rollback(f.ctx, "alpha", "20260101T000000Z-abcdef", "test"); httpCode(err) != "not_found" {
		t.Fatalf("rollback to unknown: %v", err)
	}
	// Prune is idempotent; other games are untouched.
	if plan, err := f.m.Prune(f.ctx, "alpha", 1, true, "test"); err != nil || len(plan.Delete) != 0 || len(plan.Prune) != 0 {
		t.Fatalf("second prune %+v %v", plan, err)
	}
	if _, err := f.m.Current(f.ctx, "beta"); !errors.Is(err, assets.ErrNoRelease) {
		t.Fatalf("beta current: %v", err)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// A storage failure at any step before the pointer switch (an asset, the
// manifest, the entry) leaves the previous release current; a retry reuses the
// objects the failed attempt uploaded; prune removes orphans of failed attempts.
func TestPublishFailureKeepsPreviousRelease(t *testing.T) {
	f := setup(t)
	id1 := f.publish(f.build(v1Files), "1").Release.ReleaseID
	f.st.takePuts()
	v2 := map[string]string{"index.html": "<html>v2</html>", "a.js": "a2", "b.js": "b2", "c.js": "c2"}
	dir := f.build(v2)
	for _, stage := range []string{"c.js", "/manifests/", "/releases/"} {
		boom := errors.New("injected storage failure")
		f.st.failOn = func(key string) error {
			if strings.Contains(key, stage) {
				return boom
			}
			return nil
		}
		f.clk.Add(time.Second)
		_, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: dir, Label: "2"})
		if !errors.Is(err, boom) {
			t.Fatalf("stage %s: err %v", stage, err)
		}
		if f.current() != id1 {
			t.Fatalf("stage %s: current changed to %s", stage, f.current())
		}
		f.verifyOK("")
		if rels, _, _ := f.m.List(f.ctx, "alpha"); len(rels) != 1 {
			t.Fatalf("stage %s: failed publish recorded a release: %+v", stage, rels)
		}
	}
	f.st.failOn = nil
	f.st.takePuts()
	r2 := f.publish(dir, "2")
	// a.js, b.js, c.js were stored by the failed attempts and are reused.
	if r2.Reused != 3 || r2.Uploaded != 2 || r2.Previous != id1 {
		t.Fatalf("retry %+v puts %v", r2, f.st.takePuts())
	}
	f.verifyOK("")
	// Orphaned manifests/entries of the failed attempts and v1-only objects go with keep=1.
	plan, err := f.m.Prune(f.ctx, "alpha", 1, true, "test")
	if err != nil || len(plan.Failures) != 0 {
		t.Fatalf("prune %+v %v", plan, err)
	}
	orphans := 0
	for _, k := range plan.Delete {
		if strings.Contains(k, "/manifests/") && !strings.Contains(k, id1) {
			orphans++
		}
	}
	if orphans != 1 { // the entry-stage failure had uploaded its manifest
		t.Fatalf("orphan manifests deleted %d: %v", orphans, plan.Delete)
	}
	f.verifyOK("")
}

// Publish and prune of one game are serialized by a lease: a concurrent
// publish fails fast with ErrBusy; a publish whose lease expired and was taken
// over cannot switch the pointer.
func TestConcurrentPublishIsSerialized(t *testing.T) {
	f := setup(t)
	f.m.LockTTL = time.Minute
	dirA := f.build(map[string]string{"index.html": "A", "x.js": "xa"})
	dirB := f.build(map[string]string{"index.html": "B", "x.js": "xb"})
	f.st.block, f.st.entered = make(chan struct{}), make(chan struct{}, 1)
	errA := make(chan error, 1)
	go func() {
		_, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: dirA, Label: "A"})
		errA <- err
	}()
	<-f.st.entered // A holds the lease and is uploading
	if _, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: dirB, Label: "B"}); !errors.Is(err, assets.ErrBusy) {
		t.Fatalf("concurrent publish: %v", err)
	}
	if _, err := f.m.Prune(f.ctx, "alpha", 1, true, "test"); !errors.Is(err, assets.ErrBusy) {
		t.Fatalf("prune during publish: %v", err)
	}
	f.st.mu.Lock()
	close(f.st.block)
	f.st.block = nil
	f.st.mu.Unlock()
	if err := <-errA; err != nil {
		t.Fatal(err)
	}
	rels, cur, _ := f.m.List(f.ctx, "alpha")
	if len(rels) != 1 || rels[0].Label != "A" || cur != rels[0].ReleaseID {
		t.Fatalf("releases %+v", rels)
	}

	// Lease takeover: A stalls past the TTL, B takes the lease and publishes; A must not switch.
	dirA2 := f.build(map[string]string{"index.html": "A2", "y.js": "ya"})
	f.st.block, f.st.entered = make(chan struct{}), make(chan struct{}, 1)
	go func() {
		_, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: dirA2, Label: "A2"})
		errA <- err
	}()
	<-f.st.entered
	f.clk.Add(2 * time.Minute)
	f.st.mu.Lock()
	gate := f.st.block
	f.st.block = nil // B's uploads proceed
	f.st.mu.Unlock()
	rb, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: dirB, Label: "B"})
	if err != nil {
		t.Fatalf("takeover publish: %v", err)
	}
	close(gate)
	if err := <-errA; !errors.Is(err, assets.ErrLeaseLost) {
		t.Fatalf("stalled publish: %v", err)
	}
	if f.current() != rb.Release.ReleaseID {
		t.Fatalf("current %s, want B %s", f.current(), rb.Release.ReleaseID)
	}
	f.verifyOK("")
}

// A content-addressed key is never overwritten with a different body.
func TestImmutableKeyNotOverwritten(t *testing.T) {
	f := setup(t)
	dir := f.build(map[string]string{"index.html": "i", "app.js": "real"})
	r1 := f.publish(dir, "1")
	// Simulate an untracked foreign object at the key a new file will use.
	dir2 := f.build(map[string]string{"index.html": "i2", "new.js": "new body"})
	key := assetKeyFor(t, "alpha", "new body", "new.js")
	if err := f.st.Local.Put(f.ctx, key, strings.NewReader("other body"), 10, ""); err != nil {
		t.Fatal(err)
	}
	f.clk.Add(time.Second)
	if _, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: dir2}); !errors.Is(err, assets.ErrImmutable) {
		t.Fatalf("err %v", err)
	}
	if f.current() != r1.Release.ReleaseID {
		t.Fatal("current changed")
	}
	if body, _ := readObject(t, f.st, key); body != "other body" {
		t.Fatalf("object overwritten: %q", body)
	}
	// An untracked object with the right body (e.g. uploaded by a crashed publish) is adopted.
	if err := f.st.Local.Put(f.ctx, key, strings.NewReader("new body"), 8, ""); err != nil {
		t.Fatal(err)
	}
	f.st.takePuts()
	r2 := f.publish(dir2, "2")
	if r2.Reused != 1 {
		t.Fatalf("not adopted: %+v", r2)
	}
}

// verify reports missing and corrupted objects; rollback refuses an incomplete release.
func TestVerifyDetectsMissingAndCorrupt(t *testing.T) {
	f := setup(t)
	r1 := f.publish(f.build(v1Files), "1")
	r2 := f.publish(f.build(map[string]string{"index.html": "v2", "js/app.js": "v2"}), "2")
	key := assetKeyFor(t, "alpha", "PNGDATA", "img/logo.png")
	if err := f.st.Delete(f.ctx, key); err != nil {
		t.Fatal(err)
	}
	lvl := assetKeyFor(t, "alpha", `{"level":1}`, "data/level.json")
	if err := f.st.Local.Put(f.ctx, lvl, strings.NewReader(`{"level":99}`), 12, ""); err != nil {
		t.Fatal(err)
	}
	rep, err := f.m.Verify(f.ctx, "alpha", r1.Release.ReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range rep.Problems {
		got[p.Path] = p.Problem
	}
	if len(rep.Problems) != 2 || got["img/logo.png"] != "missing" || got["data/level.json"] != "size_mismatch" {
		t.Fatalf("problems %+v", rep.Problems)
	}
	if _, err := f.m.Rollback(f.ctx, "alpha", r1.Release.ReleaseID, "t"); httpCode(err) != "release_incomplete" {
		t.Fatalf("rollback to incomplete: %v", err)
	}
	if f.current() != r2.Release.ReleaseID {
		t.Fatal("current changed")
	}
	// Publishing over a recorded object whose stored size differs is refused,
	// never overwritten; once the operator removes it, the same build republishes
	// and both objects are uploaded again.
	f.clk.Add(time.Second)
	if _, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: f.build(v1Files)}); !errors.Is(err, assets.ErrImmutable) {
		t.Fatalf("republish over corrupt object: %v", err)
	}
	if err := f.st.Delete(f.ctx, lvl); err != nil {
		t.Fatal(err)
	}
	f.st.takePuts()
	r3 := f.publish(f.build(v1Files), "repair")
	if !f.exists(key) || !f.exists(lvl) || r3.Reused != 1 { // js/app.js v1 still present
		t.Fatalf("repair %+v puts %v", r3, f.st.takePuts())
	}
	f.verifyOK("")
}

func TestPublishInputValidation(t *testing.T) {
	f := setup(t)
	if _, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: f.build(map[string]string{"main.html": "x"})}); err == nil ||
		!strings.Contains(err.Error(), "entry") {
		t.Fatalf("missing entry: %v", err)
	}
	if _, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "nope", Dir: f.build(v1Files)}); err == nil {
		t.Fatal("unknown game accepted")
	}
	dir := f.build(v1Files)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: dir}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := f.m.Publish(f.ctx, assets.PublishOptions{GameID: "alpha", Dir: f.build(v1Files), Label: "bad\nlabel"}); err == nil {
		t.Fatal("control character in label accepted")
	}
	// Names outside the key alphabet are mapped, paths are kept in the manifest.
	r := f.publish(f.build(map[string]string{"index.html": "i", "音乐/主题 曲.mp3": "mp3"}), "u")
	rep := f.verifyOK(r.Release.ReleaseID)
	if rep.Checked != 3 {
		t.Fatalf("checked %d", rep.Checked)
	}
	if _, err := f.m.Prune(f.ctx, "alpha", 0, false, "t"); err == nil {
		t.Fatal("keep 0 accepted")
	}
}

func assetKeyFor(t *testing.T, gameID, body, rel string) string {
	t.Helper()
	h := sha256.Sum256([]byte(body))
	return "games/" + gameID + "/assets/" + hex.EncodeToString(h[:]) + "/" + filepath.Base(rel)
}

func readObject(t *testing.T, s storage.Storage, key string) (string, storage.Info) {
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
