package assets_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/modules/assets"
	"github.com/pillar/gostack/modules/storage"
)

type httpEnv struct {
	*gametest.Env
	cdn, presign *assets.Module
}

func newHTTPEnv(t *testing.T) *httpEnv {
	t.Helper()
	l, err := storage.NewLocal(storage.LocalConfig{Root: t.TempDir(), PublicURL: "/v1/files", Secret: bytes.Repeat([]byte("k"), 32)})
	if err != nil {
		t.Fatal(err)
	}
	h := &httpEnv{}
	h.Env = gametest.New(t, gametest.Options{Routes: func(mux *http.ServeMux, a *app.App) {
		h.cdn = &assets.Module{Kit: a.Game, DB: a.DB, Storage: l, PublicBaseURL: "https://cdn.example.com/static"}
		h.cdn.Routes(mux)
		// A second instance without a public base URL, for the presigned entry redirect.
		h.presign = &assets.Module{Kit: a.Game, DB: a.DB, Storage: l}
		inner := http.NewServeMux()
		h.presign.Routes(inner)
		mux.Handle("/presign/", http.StripPrefix("/presign", inner))
		mux.Handle("/v1/files/", l.Handler())
	}})
	return h
}

// raw sends a request without following redirects.
func (h *httpEnv) raw(method, path, token string, body string, hdr ...string) (*http.Response, []byte) {
	h.T.Helper()
	req, _ := http.NewRequest(method, h.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		h.T.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func writeBuild(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(fp), 0o755)
		if err := os.WriteFile(fp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestManifestEntryAndAdminRoutes(t *testing.T) {
	h := newHTTPEnv(t)
	ctx := context.Background()
	if r := h.Do("GET", "/v1/games/alpha/assets/manifest", "", nil); r.Status != 404 || r.Code() != "not_found" {
		t.Fatalf("no release: %d %s", r.Status, r.Raw)
	}
	if r := h.Do("GET", "/v1/games/alpha/assets/entry", "", nil); r.Status != 404 {
		t.Fatalf("no release entry: %d", r.Status)
	}
	r1, err := h.cdn.Publish(ctx, assets.PublishOptions{GameID: "alpha", Label: "1.0",
		Dir: writeBuild(t, map[string]string{"index.html": "<p>one</p>", "js/app.js": "one()"})})
	if err != nil {
		t.Fatal(err)
	}
	id1 := r1.Release.ReleaseID

	res, body := h.raw("GET", "/v1/games/alpha/assets/manifest", "", "")
	if res.StatusCode != 200 || res.Header.Get("ETag") != `"`+id1+`"` || res.Header.Get("Cache-Control") != "public, max-age=60" ||
		res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("manifest %d %v %s", res.StatusCode, res.Header, body)
	}
	var man assets.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		t.Fatal(err)
	}
	app := man.Files["js/app.js"]
	if man.ReleaseID != id1 || man.Label != "1.0" || man.BaseURL != "https://cdn.example.com/static/" ||
		app.URL != "https://cdn.example.com/static/"+app.Key || !strings.HasPrefix(app.Key, "games/alpha/assets/") ||
		man.Entry.Path != "index.html" || man.Entry.URL != "https://cdn.example.com/static/"+r1.Release.EntryKey || len(man.Files) != 1 {
		t.Fatalf("manifest %+v", man)
	}
	if res, _ := h.raw("GET", "/v1/games/alpha/assets/manifest", "", "", "If-None-Match", `W/"`+id1+`"`); res.StatusCode != 304 ||
		res.Header.Get("ETag") != `"`+id1+`"` {
		t.Fatalf("conditional manifest %d", res.StatusCode)
	}
	res, _ = h.raw("GET", "/v1/games/alpha/assets/entry", "", "")
	if res.StatusCode != 302 || res.Header.Get("Location") != "https://cdn.example.com/static/"+r1.Release.EntryKey ||
		res.Header.Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("entry %d %v", res.StatusCode, res.Header)
	}
	// Without a public base URL the entry redirects to a short-lived presigned URL.
	res, _ = h.raw("GET", "/presign/v1/games/alpha/assets/entry", "", "")
	loc := res.Header.Get("Location")
	if res.StatusCode != 302 || res.Header.Get("Cache-Control") != "no-store" || !strings.Contains(loc, "X-GS-Signature=") {
		t.Fatalf("presigned entry %d %v", res.StatusCode, res.Header)
	}
	if res, b := h.raw("GET", loc, "", ""); res.StatusCode != 200 || string(b) != "<p>one</p>" {
		t.Fatalf("entry body %d %q", res.StatusCode, b)
	}
	if res, b := h.raw("GET", "/presign/v1/games/alpha/assets/manifest", "", ""); res.StatusCode != 200 || strings.Contains(string(b), `"url"`) {
		t.Fatalf("manifest without base url has urls: %s", b)
	}
	// Other games are separate.
	if r := h.Do("GET", "/v1/games/beta/assets/manifest", "", nil); r.Status != 404 {
		t.Fatalf("beta %d", r.Status)
	}

	r2, err := h.cdn.Publish(ctx, assets.PublishOptions{GameID: "alpha", Label: "2.0",
		Dir: writeBuild(t, map[string]string{"index.html": "<p>two</p>", "js/app.js": "two()"})})
	if err != nil {
		t.Fatal(err)
	}
	id2 := r2.Release.ReleaseID
	if res, _ := h.raw("GET", "/v1/games/alpha/assets/manifest", "", "", "If-None-Match", `"`+id1+`"`); res.StatusCode != 200 ||
		res.Header.Get("ETag") != `"`+id2+`"` {
		t.Fatalf("after publish %d %v", res.StatusCode, res.Header)
	}

	// Admin list and rollback.
	l := h.Expect(h.Do("GET", "/v1/admin/games/alpha/assets/releases", h.AdminToken, nil), 200)
	rels := l.Body["releases"].([]any)
	if l.Body["current_release_id"] != id2 || len(rels) != 2 || rels[0].(map[string]any)["release_id"] != id2 {
		t.Fatalf("list %s", l.Raw)
	}
	h.Expect(h.Do("GET", "/v1/admin/games/alpha/assets/releases", "", nil), 401)
	rb := h.Expect(h.Do("POST", "/v1/admin/games/alpha/assets/rollback", h.AdminToken, map[string]string{"release_id": id1}), 200)
	if rb.Body["previous_release_id"] != id2 || rb.Body["current_release_id"] != id1 {
		t.Fatalf("rollback %s", rb.Raw)
	}
	res, body = h.raw("GET", "/v1/games/alpha/assets/manifest", "", "")
	if res.Header.Get("ETag") != `"`+id1+`"` || !strings.Contains(string(body), id1) {
		t.Fatalf("manifest after rollback %v %s", res.Header, body)
	}
	if r := h.Do("POST", "/v1/admin/games/alpha/assets/rollback", h.AdminToken, map[string]string{"release_id": "20200101T000000Z-000000"}); r.Status != 404 {
		t.Fatalf("unknown release %d", r.Status)
	}
	if r := h.Do("POST", "/v1/admin/games/beta/assets/rollback", h.AdminToken, map[string]string{"release_id": id2}); r.Status != 404 {
		t.Fatalf("cross-game release %d", r.Status)
	}
	if r := h.Do("POST", "/v1/admin/games/alpha/assets/rollback", h.AdminToken, `{"release_id":1}`); r.Status != 400 {
		t.Fatalf("bad body %d", r.Status)
	}
	if r := h.Do("POST", "/v1/admin/games/alpha/assets/rollback", h.AdminToken, `{}`); r.Status != 400 {
		t.Fatalf("empty body %d", r.Status)
	}
	// Roll forward to id2 (newest), prune with keep=1 (drops id1); rollback to id1 is refused.
	h.Expect(h.Do("POST", "/v1/admin/games/alpha/assets/rollback", h.AdminToken, map[string]string{"release_id": id2}), 200)
	if p, err := h.cdn.Prune(ctx, "alpha", 1, true, "test"); err != nil || len(p.Prune) != 1 || p.Prune[0] != id1 {
		t.Fatalf("prune %+v %v", p, err)
	}
	if r := h.Do("POST", "/v1/admin/games/alpha/assets/rollback", h.AdminToken, map[string]string{"release_id": id1}); r.Status != 409 || r.Code() != "release_pruned" {
		t.Fatalf("pruned %d %s", r.Status, r.Raw)
	}
}
