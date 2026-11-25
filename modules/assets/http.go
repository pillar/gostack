package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
)

// Routes registers the public manifest/entry routes and the admin list/rollback
// routes (game module "assets").
func (m *Module) Routes(mux *http.ServeMux) {
	k := m.Kit
	k.Handle(mux, "GET /assets/manifest", game.Public, m.getManifest)
	k.Handle(mux, "GET /assets/entry", game.Public, m.getEntry)
	k.HandleAdmin(mux, "GET /assets/releases", m.adminList)
	k.HandleAdmin(mux, "POST /assets/rollback", m.adminRollback)
}

func (m *Module) maxAge() int {
	if m.ManifestMaxAge > 0 {
		return int(m.ManifestMaxAge / time.Second)
	}
	if m.ManifestMaxAge < 0 {
		return 0
	}
	return 60
}

func (m *Module) baseURL() string {
	if m.PublicBaseURL == "" {
		return ""
	}
	return strings.TrimSuffix(m.PublicBaseURL, "/") + "/"
}

func (m *Module) current(ctx context.Context) (Release, error) {
	r, err := m.Current(ctx, game.CurrentGame(ctx).ID)
	if errors.Is(err, ErrNoRelease) {
		return r, httpx.NotFound("no asset release published")
	}
	return r, err
}

func etagMatch(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// getManifest serves the current release's manifest with absolute URLs (when
// a public base URL is configured), ETag = release id and a short public cache.
func (m *Module) getManifest(w http.ResponseWriter, r *http.Request) error {
	rel, err := m.current(r.Context())
	if err != nil {
		return err
	}
	etag := `"` + rel.ReleaseID + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", m.maxAge()))
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatch(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	body, err := m.rendered(r.Context(), rel)
	if err != nil {
		return err
	}
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

// rendered returns the API form of a release manifest. Manifests are immutable
// per key, so the rendering is cached (bounded) for the process lifetime.
func (m *Module) rendered(ctx context.Context, rel Release) ([]byte, error) {
	m.mu.Lock()
	b, ok := m.cache[rel.ManifestKey]
	m.mu.Unlock()
	if ok {
		return b, nil
	}
	man, _, err := m.loadManifest(ctx, rel.ManifestKey)
	if err != nil {
		return nil, fmt.Errorf("assets: load manifest %s: %w", rel.ManifestKey, err)
	}
	if base := m.baseURL(); base != "" {
		man.BaseURL = base
		man.Entry.URL = base + man.Entry.Key
		for p, f := range man.Files {
			f.URL = base + f.Key
			man.Files[p] = f
		}
	}
	if b, err = json.Marshal(man); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.cache == nil || len(m.cache) >= 32 {
		m.cache = map[string][]byte{}
	}
	m.cache[rel.ManifestKey] = b
	m.mu.Unlock()
	return b, nil
}

// getEntry redirects to the current release's entry file: the public base URL
// (short public cache) or, without one, a presigned storage URL (no-store).
func (m *Module) getEntry(w http.ResponseWriter, r *http.Request) error {
	rel, err := m.current(r.Context())
	if err != nil {
		return err
	}
	var loc string
	if base := m.baseURL(); base != "" {
		loc = base + rel.EntryKey
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", m.maxAge()))
	} else {
		ttl := m.EntryURLTTL
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		if loc, err = m.Storage.Presign(r.Context(), http.MethodGet, rel.EntryKey, ttl); err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusFound)
	return nil
}

func (m *Module) adminList(w http.ResponseWriter, r *http.Request) error {
	rels, cur, err := m.List(r.Context(), game.CurrentGame(r.Context()).ID)
	if err != nil {
		return err
	}
	if len(rels) > 200 {
		rels = rels[:200]
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"current_release_id": cur, "releases": rels})
	return nil
}

func (m *Module) adminRollback(w http.ResponseWriter, r *http.Request) error {
	b, err := game.ReadBody(r, 4<<10)
	if err != nil {
		return err
	}
	var in struct {
		ReleaseID string `json:"release_id"`
	}
	if err := game.DecodeStrict(b, &in); err != nil {
		return err
	}
	if in.ReleaseID == "" {
		return httpx.BadRequest("release_id is required")
	}
	ctx := r.Context()
	prev, err := m.Rollback(ctx, game.CurrentGame(ctx).ID, in.ReleaseID, "apikey:"+game.AdminKey(ctx).ID)
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"current_release_id": in.ReleaseID, "previous_release_id": prev})
	return nil
}
