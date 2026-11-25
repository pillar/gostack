package web

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/jobs"
)

// Page statuses.
const (
	StatusDraft     = "draft"
	StatusScheduled = "scheduled"
	StatusPublished = "published"
	StatusArchived  = "archived"
)

const (
	maxTitle    = 300
	maxBodyText = 256 << 10
	maxSEOText  = 1000
	maxURL      = 2048
	maxSlug     = 200
	maxSitemap  = 50000
)

// Site is a tenant's public URL space.
type Site struct {
	ID             string   `json:"id"`
	TenantID       string   `json:"tenant_id"`
	Name           string   `json:"name"`
	BaseURL        string   `json:"base_url"`
	RobotsDisallow []string `json:"robots_disallow"`
	CreatedAt      int64    `json:"created_at"`
}

// SEO is the per-revision metadata.
type SEO struct {
	Title         string `json:"title"`
	Description   string `json:"description"`
	CanonicalURL  string `json:"canonical_url"`
	OGTitle       string `json:"og_title"`
	OGDescription string `json:"og_description"`
	OGImage       string `json:"og_image"`
	NoIndex       bool   `json:"noindex"`
}

// Revision is an immutable content version.
type Revision struct {
	Rev       int64  `json:"rev"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	SEO       SEO    `json:"seo"`
	Author    string `json:"author"`
	CreatedAt int64  `json:"created_at"`
}

// Page is a page or post with its lifecycle state.
type Page struct {
	ID                string    `json:"id"`
	SiteID            string    `json:"site_id"`
	Kind              string    `json:"kind"`
	Slug              string    `json:"slug"`
	Status            string    `json:"status"`
	DraftRevision     int64     `json:"draft_revision"`
	PublishedRevision int64     `json:"published_revision"`
	ScheduledRevision int64     `json:"scheduled_revision"`
	PublishAt         *int64    `json:"publish_at,omitempty"`
	PublishedAt       *int64    `json:"published_at,omitempty"`
	Version           int64     `json:"version"`
	CreatedAt         int64     `json:"created_at"`
	UpdatedAt         int64     `json:"updated_at"`
	Draft             *Revision `json:"draft,omitempty"`
}

func validSiteID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validSlug: "/"-separated segments of [a-z0-9-], 1-200 bytes.
func validSlug(s string) bool {
	if len(s) == 0 || len(s) > maxSlug {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if !validSiteID(seg) {
			return false
		}
	}
	return true
}

func validAbsURL(s string) bool {
	if len(s) > maxURL {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

func (s SEO) validate() error {
	if len(s.Title) > maxTitle || len(s.OGTitle) > maxTitle || len(s.Description) > maxSEOText || len(s.OGDescription) > maxSEOText {
		return httpx.BadRequest("seo titles at most 300 bytes, descriptions at most 1000 bytes")
	}
	for _, u := range []string{s.CanonicalURL, s.OGImage} {
		if u != "" && !validAbsURL(u) {
			return httpx.BadRequest("seo canonical_url and og_image must be absolute http(s) URLs")
		}
	}
	return nil
}

// CreateSite registers a site (its id is global: it appears in public URLs).
func (k *Kit) CreateSite(ctx context.Context, tenant string, s Site) (Site, error) {
	if !validSiteID(s.ID) {
		return Site{}, httpx.BadRequest("site id must be 1-64 chars [a-z0-9-]")
	}
	if s.Name == "" || len(s.Name) > maxTitle {
		return Site{}, httpx.BadRequest("name required (at most 300 bytes)")
	}
	u, err := url.Parse(s.BaseURL)
	if !validAbsURL(s.BaseURL) || err != nil || u.RawQuery != "" || u.Fragment != "" {
		return Site{}, httpx.BadRequest("base_url must be an absolute http(s) URL without query")
	}
	if k.Production && u.Scheme != "https" {
		return Site{}, httpx.BadRequest("production: base_url must use https")
	}
	s.BaseURL = strings.TrimSuffix(s.BaseURL, "/")
	if len(s.RobotsDisallow) > 100 {
		return Site{}, httpx.BadRequest("at most 100 robots_disallow entries")
	}
	for _, p := range s.RobotsDisallow {
		if !strings.HasPrefix(p, "/") || len(p) > 200 || strings.ContainsAny(p, "\r\n") {
			return Site{}, httpx.BadRequest("robots_disallow entries must be paths starting with /")
		}
	}
	if s.RobotsDisallow == nil {
		s.RobotsDisallow = []string{}
	}
	s.TenantID, s.CreatedAt = tenant, k.now().UnixMilli()
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM web_sites WHERE id=?`, s.ID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return httpx.Conflict("conflict", "site id already exists")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO web_sites(id,tenant_id,name,base_url,robots_disallow,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
			s.ID, tenant, s.Name, s.BaseURL, strings.Join(s.RobotsDisallow, "\n"), s.CreatedAt, s.CreatedAt)
		return err
	})
	return s, err
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func scanSite(row interface{ Scan(...any) error }) (Site, error) {
	var s Site
	var dis string
	err := row.Scan(&s.ID, &s.TenantID, &s.Name, &s.BaseURL, &dis, &s.CreatedAt)
	s.RobotsDisallow = []string{}
	if dis != "" {
		s.RobotsDisallow = strings.Split(dis, "\n")
	}
	return s, err
}

// site loads a site; tenant "" matches any tenant (public routes).
func (k *Kit) site(ctx context.Context, q querier, tenant, id string) (Site, error) {
	s, err := scanSite(q.QueryRowContext(ctx, `SELECT id,tenant_id,name,base_url,robots_disallow,created_at FROM web_sites WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && tenant != "" && s.TenantID != tenant) {
		return Site{}, httpx.NotFound("site not found")
	}
	return s, err
}

// ListSites returns the tenant's sites.
func (k *Kit) ListSites(ctx context.Context, tenant string) ([]Site, error) {
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT id,tenant_id,name,base_url,robots_disallow,created_at FROM web_sites WHERE tenant_id=? ORDER BY id`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Site{}
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PageInput creates or edits a page. ExpectedVersion (edits) guards against
// lost updates: 409 revision_conflict when the page changed meanwhile.
type PageInput struct {
	Kind            string `json:"kind"`
	Slug            string `json:"slug"`
	Title           string `json:"title"`
	Body            string `json:"body"`
	SEO             SEO    `json:"seo"`
	ExpectedVersion int64  `json:"expected_version"`
}

func (in *PageInput) validate(create bool) error {
	if create {
		if in.Kind == "" {
			in.Kind = "page"
		}
		if in.Kind != "page" && in.Kind != "post" {
			return httpx.BadRequest("kind must be page or post")
		}
	} else if in.Kind != "" {
		return httpx.BadRequest("kind cannot be changed")
	}
	if !validSlug(in.Slug) {
		return httpx.BadRequest("slug must be 1-200 bytes of /-separated [a-z0-9-] segments")
	}
	if in.Title == "" || len(in.Title) > maxTitle {
		return httpx.BadRequest("title required (at most 300 bytes)")
	}
	if len(in.Body) > maxBodyText {
		return httpx.BadRequest("body at most 256 KiB")
	}
	return in.SEO.validate()
}

func insertRevision(ctx context.Context, tx *sql.Tx, tenant, page string, rev int64, in PageInput, author string, now int64) error {
	noindex := 0
	if in.SEO.NoIndex {
		noindex = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO web_page_revisions(tenant_id,page_id,rev,title,body,seo_title,seo_description,canonical_url,og_title,og_description,og_image,noindex,author,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, tenant, page, rev, in.Title, in.Body, in.SEO.Title, in.SEO.Description, in.SEO.CanonicalURL,
		in.SEO.OGTitle, in.SEO.OGDescription, in.SEO.OGImage, noindex, author, now)
	return err
}

func slugTaken(ctx context.Context, tx *sql.Tx, tenant, site, slug, except string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM web_pages WHERE tenant_id=? AND site_id=? AND slug=? AND id<>?`, tenant, site, slug, except).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return httpx.Conflict("slug_taken", "another page of this site uses the slug")
	}
	return nil
}

// CreatePage creates a draft with revision 1.
func (k *Kit) CreatePage(ctx context.Context, tenant, siteID, author string, in PageInput) (Page, error) {
	if err := in.validate(true); err != nil {
		return Page{}, err
	}
	var p Page
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := k.site(ctx, tx, tenant, siteID); err != nil {
			return err
		}
		if err := slugTaken(ctx, tx, tenant, siteID, in.Slug, ""); err != nil {
			return err
		}
		now := k.now().UnixMilli()
		id := httpx.RandomID("pg_")
		if _, err := tx.ExecContext(ctx, `INSERT INTO web_pages(tenant_id,id,site_id,kind,slug,status,draft_revision,created_at,updated_at)
			VALUES(?,?,?,?,?,?,1,?,?)`, tenant, id, siteID, in.Kind, in.Slug, StatusDraft, now, now); err != nil {
			return err
		}
		if err := insertRevision(ctx, tx, tenant, id, 1, in, author, now); err != nil {
			return err
		}
		var err error
		p, err = k.page(ctx, tx, tenant, id, true)
		return err
	})
	return p, err
}

const pageCols = `id,site_id,kind,slug,status,draft_revision,published_revision,scheduled_revision,publish_at,published_at,version,created_at,updated_at`

func scanPage(row interface{ Scan(...any) error }) (Page, error) {
	var p Page
	err := row.Scan(&p.ID, &p.SiteID, &p.Kind, &p.Slug, &p.Status, &p.DraftRevision, &p.PublishedRevision, &p.ScheduledRevision,
		&p.PublishAt, &p.PublishedAt, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (k *Kit) page(ctx context.Context, q querier, tenant, id string, withDraft bool) (Page, error) {
	p, err := scanPage(q.QueryRowContext(ctx, `SELECT `+pageCols+` FROM web_pages WHERE tenant_id=? AND id=?`, tenant, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Page{}, httpx.NotFound("page not found")
	}
	if err != nil || !withDraft {
		return p, err
	}
	r, err := revision(ctx, q, tenant, id, p.DraftRevision)
	p.Draft = &r
	return p, err
}

const revCols = `rev,title,body,seo_title,seo_description,canonical_url,og_title,og_description,og_image,noindex,author,created_at`

func scanRevision(row interface{ Scan(...any) error }) (Revision, error) {
	var r Revision
	var noindex int64
	err := row.Scan(&r.Rev, &r.Title, &r.Body, &r.SEO.Title, &r.SEO.Description, &r.SEO.CanonicalURL, &r.SEO.OGTitle,
		&r.SEO.OGDescription, &r.SEO.OGImage, &noindex, &r.Author, &r.CreatedAt)
	r.SEO.NoIndex = noindex != 0
	return r, err
}

func revision(ctx context.Context, q querier, tenant, page string, rev int64) (Revision, error) {
	r, err := scanRevision(q.QueryRowContext(ctx, `SELECT `+revCols+` FROM web_page_revisions WHERE tenant_id=? AND page_id=? AND rev=?`, tenant, page, rev))
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, httpx.NotFound("revision not found")
	}
	return r, err
}

// GetPage returns a page with its current draft revision.
func (k *Kit) GetPage(ctx context.Context, tenant, id string) (Page, error) {
	return k.page(ctx, k.DB.Read, tenant, id, true)
}

// ListPages lists a site's pages (optionally by status), ordered by slug.
func (k *Kit) ListPages(ctx context.Context, tenant, siteID, status string) ([]Page, error) {
	if _, err := k.site(ctx, k.DB.Read, tenant, siteID); err != nil {
		return nil, err
	}
	q := `SELECT ` + pageCols + ` FROM web_pages WHERE tenant_id=? AND site_id=?`
	args := []any{tenant, siteID}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	rows, err := k.DB.Read.QueryContext(ctx, q+` ORDER BY slug LIMIT 1000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Page{}
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Revisions lists a page's revisions, newest first.
func (k *Kit) Revisions(ctx context.Context, tenant, id string) ([]Revision, error) {
	if _, err := k.page(ctx, k.DB.Read, tenant, id, false); err != nil {
		return nil, err
	}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT `+revCols+` FROM web_page_revisions WHERE tenant_id=? AND page_id=? ORDER BY rev DESC LIMIT 200`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdatePage appends a revision and makes it the draft. A published page
// keeps serving its published revision until it is published again.
func (k *Kit) UpdatePage(ctx context.Context, tenant, id, author string, in PageInput) (Page, error) {
	if err := in.validate(false); err != nil {
		return Page{}, err
	}
	var p Page
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := k.page(ctx, tx, tenant, id, false)
		if err != nil {
			return err
		}
		if in.ExpectedVersion != cur.Version {
			return httpx.Conflict("revision_conflict", fmt.Sprintf("page is at version %d", cur.Version))
		}
		if err := slugTaken(ctx, tx, tenant, cur.SiteID, in.Slug, id); err != nil {
			return err
		}
		now := k.now().UnixMilli()
		rev := cur.DraftRevision + 1
		if err := insertRevision(ctx, tx, tenant, id, rev, in, author, now); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE web_pages SET slug=?, draft_revision=?, version=version+1, updated_at=? WHERE tenant_id=? AND id=? AND version=?`,
			in.Slug, rev, now, tenant, id, cur.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return httpx.Conflict("revision_conflict", "page changed concurrently")
		}
		p, err = k.page(ctx, tx, tenant, id, true)
		return err
	})
	return p, err
}

func errTransition(from, action string) error {
	return httpx.Conflict("invalid_transition", fmt.Sprintf("cannot %s a %s page", action, from))
}

// Publish publishes revision rev (0 = the current draft) now, or schedules it
// when at is in the future (a jobs entry publishes it then). Publishing the
// revision that is already live is a no-op.
//
//	draft|archived  -> published | scheduled
//	scheduled       -> published | scheduled (rescheduled)
//	published       -> published (another revision)
func (k *Kit) Publish(ctx context.Context, tenant, id string, rev int64, at time.Time) (Page, error) {
	var p Page
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := k.page(ctx, tx, tenant, id, false)
		if err != nil {
			return err
		}
		// fn may re-run (SQLITE_BUSY / serialization failure): resolve
		// "current draft" into a local on every attempt, never into rev.
		rev := rev
		if rev == 0 {
			rev = cur.DraftRevision
		}
		if rev < 1 || rev > cur.DraftRevision {
			return httpx.BadRequest("unknown revision")
		}
		now := k.now()
		nowMs := now.UnixMilli()
		future := !at.IsZero() && at.After(now)
		switch {
		case future && cur.Status == StatusPublished:
			return httpx.Conflict("invalid_transition", "a published page cannot be scheduled; archive it first or publish now")
		case !future && cur.Status == StatusPublished && cur.PublishedRevision == rev:
			// already live: idempotent
		case future:
			token := httpx.RandomID("st_")
			if _, err := tx.ExecContext(ctx, `UPDATE web_pages SET status=?, scheduled_revision=?, publish_at=?, schedule_token=?, version=version+1, updated_at=?
				WHERE tenant_id=? AND id=?`, StatusScheduled, rev, at.UnixMilli(), token, nowMs, tenant, id); err != nil {
				return err
			}
			b, _ := json.Marshal(publishJob{PageID: id, Token: token})
			if _, err := k.Jobs.Enqueue(ctx, tx, jobs.Job{TenantID: tenant, Type: JobPublish, Payload: b, RunAt: at, DedupeKey: "publish:" + id + ":" + token}); err != nil {
				return err
			}
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE web_pages SET status=?, published_revision=?, published_at=?, scheduled_revision=0, publish_at=NULL,
				schedule_token='', version=version+1, updated_at=? WHERE tenant_id=? AND id=?`, StatusPublished, rev, nowMs, nowMs, tenant, id); err != nil {
				return err
			}
		}
		p, err = k.page(ctx, tx, tenant, id, true)
		return err
	})
	return p, err
}

// Unschedule returns a scheduled page to draft; its pending job becomes a no-op.
func (k *Kit) Unschedule(ctx context.Context, tenant, id string) (Page, error) {
	return k.simpleTransition(ctx, tenant, id, "unschedule", map[string]bool{StatusScheduled: true}, StatusDraft)
}

// Archive takes a page offline (idempotent).
func (k *Kit) Archive(ctx context.Context, tenant, id string) (Page, error) {
	return k.simpleTransition(ctx, tenant, id, "archive", map[string]bool{StatusDraft: true, StatusScheduled: true, StatusPublished: true}, StatusArchived)
}

func (k *Kit) simpleTransition(ctx context.Context, tenant, id, action string, from map[string]bool, to string) (Page, error) {
	var p Page
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := k.page(ctx, tx, tenant, id, false)
		if err != nil {
			return err
		}
		if cur.Status != to {
			if !from[cur.Status] {
				return errTransition(cur.Status, action)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE web_pages SET status=?, scheduled_revision=0, publish_at=NULL, schedule_token='', version=version+1, updated_at=?
				WHERE tenant_id=? AND id=?`, to, k.now().UnixMilli(), tenant, id); err != nil {
				return err
			}
		}
		p, err = k.page(ctx, tx, tenant, id, true)
		return err
	})
	return p, err
}

type publishJob struct {
	PageID string `json:"page_id"`
	Token  string `json:"token"`
}

// handlePublish publishes a scheduled page. Stale jobs (unscheduled,
// rescheduled, archived) do nothing; a repeated job is harmless.
func (k *Kit) handlePublish(ctx context.Context, c *jobs.Claimed) error {
	var p publishJob
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		return nil
	}
	return k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		now := k.now().UnixMilli()
		res, err := tx.ExecContext(ctx, `UPDATE web_pages SET status=?, published_revision=scheduled_revision, published_at=?, scheduled_revision=0,
			publish_at=NULL, schedule_token='', version=version+1, updated_at=?
			WHERE tenant_id=? AND id=? AND status=? AND schedule_token=? AND publish_at<=?`,
			StatusPublished, now, now, c.TenantID, p.PageID, StatusScheduled, p.Token, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var at sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT publish_at FROM web_pages WHERE tenant_id=? AND id=? AND status=? AND schedule_token=?`,
				c.TenantID, p.PageID, StatusScheduled, p.Token).Scan(&at)
			if err == nil && at.Valid && at.Int64 > now {
				return fmt.Errorf("web: page %s is not due yet", p.PageID) // early (clock skew): retry later
			}
		}
		return nil
	})
}

// PublicPage is the public representation of a published page.
type PublicPage struct {
	Site        string `json:"site"`
	Kind        string `json:"kind"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	PublishedAt int64  `json:"published_at"`
	SEO         SEO    `json:"seo"`
}

func pageURL(s Site, slug string) string { return s.BaseURL + "/" + slug }

// PublishedPage resolves a published page by site and slug.
func (k *Kit) PublishedPage(ctx context.Context, siteID, slug string) (PublicPage, error) {
	s, err := k.site(ctx, k.DB.Read, "", siteID)
	if err != nil {
		return PublicPage{}, err
	}
	var pp PublicPage
	var r Revision
	var noindex int64
	err = k.DB.Read.QueryRowContext(ctx, `SELECT p.kind, p.slug, p.published_at, r.title, r.body, r.seo_title, r.seo_description, r.canonical_url,
		r.og_title, r.og_description, r.og_image, r.noindex
		FROM web_pages p JOIN web_page_revisions r ON r.tenant_id=p.tenant_id AND r.page_id=p.id AND r.rev=p.published_revision
		WHERE p.tenant_id=? AND p.site_id=? AND p.slug=? AND p.status=?`, s.TenantID, s.ID, slug, StatusPublished).
		Scan(&pp.Kind, &pp.Slug, &pp.PublishedAt, &r.Title, &r.Body, &r.SEO.Title, &r.SEO.Description, &r.SEO.CanonicalURL,
			&r.SEO.OGTitle, &r.SEO.OGDescription, &r.SEO.OGImage, &noindex)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicPage{}, httpx.NotFound("page not found")
	}
	if err != nil {
		return PublicPage{}, err
	}
	pp.Site, pp.Title, pp.Body, pp.SEO = s.ID, r.Title, r.Body, r.SEO
	pp.SEO.NoIndex = noindex != 0
	// Effective SEO values: fall back to the title and the site URL.
	if pp.SEO.Title == "" {
		pp.SEO.Title = r.Title
	}
	if pp.SEO.CanonicalURL == "" {
		pp.SEO.CanonicalURL = pageURL(s, pp.Slug)
	}
	if pp.SEO.OGTitle == "" {
		pp.SEO.OGTitle = pp.SEO.Title
	}
	if pp.SEO.OGDescription == "" {
		pp.SEO.OGDescription = pp.SEO.Description
	}
	return pp, nil
}

// writeCached writes body with a strong ETag and Cache-Control, answering
// If-None-Match with 304.
func (k *Kit) writeCached(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(k.Cfg.PublicMaxAge/time.Second)))
	for _, t := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if t = strings.TrimSpace(t); t == etag || t == "W/"+etag || t == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (k *Kit) httpPublicPage(w http.ResponseWriter, r *http.Request) error {
	p, err := k.PublishedPage(r.Context(), r.PathValue("site"), r.PathValue("slug"))
	if err != nil {
		return err
	}
	if p.SEO.NoIndex {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	k.writeCached(w, r, "application/json", append(b, '\n'))
	return nil
}

// Sitemap renders sitemap.xml for a site: published pages without noindex.
func (k *Kit) Sitemap(ctx context.Context, siteID string) ([]byte, error) {
	s, err := k.site(ctx, k.DB.Read, "", siteID)
	if err != nil {
		return nil, err
	}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT p.slug, p.published_at, r.canonical_url
		FROM web_pages p JOIN web_page_revisions r ON r.tenant_id=p.tenant_id AND r.page_id=p.id AND r.rev=p.published_revision
		WHERE p.tenant_id=? AND p.site_id=? AND p.status=? AND r.noindex=0 ORDER BY p.slug LIMIT ?`, s.TenantID, s.ID, StatusPublished, maxSitemap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type urlEntry struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	}
	set := struct {
		XMLName xml.Name   `xml:"urlset"`
		NS      string     `xml:"xmlns,attr"`
		URLs    []urlEntry `xml:"url"`
	}{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for rows.Next() {
		var slug, canonical string
		var at int64
		if err := rows.Scan(&slug, &at, &canonical); err != nil {
			return nil, err
		}
		loc := canonical
		if loc == "" {
			loc = pageURL(s, slug)
		}
		set.URLs = append(set.URLs, urlEntry{Loc: loc, LastMod: time.UnixMilli(at).UTC().Format(time.RFC3339)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	b, err := xml.MarshalIndent(set, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(b, '\n')...), nil
}

func (k *Kit) httpSitemap(w http.ResponseWriter, r *http.Request) error {
	b, err := k.Sitemap(r.Context(), r.PathValue("site"))
	if err != nil {
		return err
	}
	k.writeCached(w, r, "application/xml; charset=utf-8", b)
	return nil
}

// Robots renders robots.txt. The sitemap is announced at <base_url>/sitemap.xml:
// the site's front end serves both files by proxying the public endpoints.
func (k *Kit) Robots(ctx context.Context, siteID string) ([]byte, error) {
	s, err := k.site(ctx, k.DB.Read, "", siteID)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	if len(s.RobotsDisallow) == 0 {
		b.WriteString("Allow: /\n")
	}
	for _, p := range s.RobotsDisallow {
		b.WriteString("Disallow: " + p + "\n")
	}
	b.WriteString("\nSitemap: " + s.BaseURL + "/sitemap.xml\n")
	return []byte(b.String()), nil
}

func (k *Kit) httpRobots(w http.ResponseWriter, r *http.Request) error {
	b, err := k.Robots(r.Context(), r.PathValue("site"))
	if err != nil {
		return err
	}
	k.writeCached(w, r, "text/plain; charset=utf-8", b)
	return nil
}

// HTTP admin handlers.

func (k *Kit) httpCreateSite(w http.ResponseWriter, r *http.Request, a actor) error {
	var s Site
	if err := httpx.DecodeJSON(r, &s); err != nil {
		return err
	}
	s, err := k.CreateSite(r.Context(), a.Tenant, s)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, s)
	return nil
}

func (k *Kit) httpListSites(w http.ResponseWriter, r *http.Request, a actor) error {
	s, err := k.ListSites(r.Context(), a.Tenant)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sites": s})
	return nil
}

func (k *Kit) httpCreatePage(w http.ResponseWriter, r *http.Request, a actor) error {
	var in PageInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	p, err := k.CreatePage(r.Context(), a.Tenant, r.PathValue("site"), a.By, in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
	return nil
}

func (k *Kit) httpListPages(w http.ResponseWriter, r *http.Request, a actor) error {
	ps, err := k.ListPages(r.Context(), a.Tenant, r.PathValue("site"), r.URL.Query().Get("status"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"pages": ps})
	return nil
}

func (k *Kit) httpGetPage(w http.ResponseWriter, r *http.Request, a actor) error {
	p, err := k.GetPage(r.Context(), a.Tenant, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, p)
	return nil
}

func (k *Kit) httpUpdatePage(w http.ResponseWriter, r *http.Request, a actor) error {
	var in PageInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	p, err := k.UpdatePage(r.Context(), a.Tenant, r.PathValue("id"), a.By, in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, p)
	return nil
}

func (k *Kit) httpRevisions(w http.ResponseWriter, r *http.Request, a actor) error {
	rs, err := k.Revisions(r.Context(), a.Tenant, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"revisions": rs})
	return nil
}

func (k *Kit) httpPublish(w http.ResponseWriter, r *http.Request, a actor) error {
	var req struct {
		Revision int64 `json:"revision"`
		// At is the publish time in unix milliseconds (0 or past = now).
		At int64 `json:"at"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	var at time.Time
	if req.At > 0 {
		at = time.UnixMilli(req.At)
	}
	p, err := k.Publish(r.Context(), a.Tenant, r.PathValue("id"), req.Revision, at)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, p)
	return nil
}

func (k *Kit) httpUnschedule(w http.ResponseWriter, r *http.Request, a actor) error {
	p, err := k.Unschedule(r.Context(), a.Tenant, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, p)
	return nil
}

func (k *Kit) httpArchive(w http.ResponseWriter, r *http.Request, a actor) error {
	p, err := k.Archive(r.Context(), a.Tenant, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, p)
	return nil
}
