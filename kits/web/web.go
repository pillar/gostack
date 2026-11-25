// Package web is WebKit P1 (PRD §24): content publishing with revisions and
// SEO metadata (public pages, sitemap.xml, robots.txt with ETag caching), an
// order state machine example on the billing module, and a webhook outbox
// delivered by the jobs queue with HMAC-signed, retried deliveries.
//
// It deliberately is not a CMS or a shop platform: there is no editor UI,
// no media library, no catalog, carts, taxes or payment provider.
//
// Admin routes accept an API key with scope "web" (tenant from the key) or,
// when auth.web is enabled, a web session (modules/webauth: cookie + CSRF
// header) of a user listed as an editor of the session's tenant. Public read
// routes need no credentials and serve published content only.
package web

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/billing"
	"github.com/pillar/gostack/modules/jobs"
	"github.com/pillar/gostack/modules/webauth"
)

// Scope is the API key scope of the admin API.
const Scope = "web"

// Job types.
const (
	JobPublish = "web.publish"
	JobWebhook = "web.webhook"
)

const maxBody = 512 << 10

// Kit is WebKit. Set the fields, then call Routes and register JobHandlers.
type Kit struct {
	DB   *store.DB
	Jobs *jobs.Queue
	Keys auth.APIKeys
	// Billing enables the order example (routes are not mounted without it).
	Billing *billing.Service
	// WebAuth enables editor sessions (nil: API keys only).
	WebAuth *webauth.Module
	Cfg     config.WebConfig
	// Production requires https webhook URLs.
	Production bool
	Now        func() time.Time
	Log        *slog.Logger
	// Client delivers webhooks (default: SSRF-guarded client, no redirects).
	Client *http.Client
}

func (k *Kit) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Kit) log() *slog.Logger {
	if k.Log != nil {
		return k.Log
	}
	return slog.Default()
}

// actor is the authenticated caller of an admin route.
type actor struct {
	Tenant string
	By     string // key:<id> or user:<id>
	Key    bool
}

// JobHandlers returns the queue handlers of the kit.
func (k *Kit) JobHandlers() map[string]jobs.Handler {
	return map[string]jobs.Handler{JobPublish: k.handlePublish, JobWebhook: k.handleWebhook}
}

// Routes registers /v1/web/*.
func (k *Kit) Routes(mux *http.ServeMux) {
	h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, a actor) error) {
		mux.Handle(pattern, k.adminHandler(fn))
	}
	g := func(pattern string, fn httpx.HandlerFunc) { mux.Handle(pattern, fn) }

	h("POST /v1/web/sites", k.httpCreateSite)
	h("GET /v1/web/sites", k.httpListSites)
	h("POST /v1/web/editors", k.httpAddEditor)
	h("DELETE /v1/web/editors/{user}", k.httpRemoveEditor)
	h("POST /v1/web/sites/{site}/pages", k.httpCreatePage)
	h("GET /v1/web/sites/{site}/pages", k.httpListPages)
	h("GET /v1/web/pages/{id}", k.httpGetPage)
	h("PUT /v1/web/pages/{id}", k.httpUpdatePage)
	h("GET /v1/web/pages/{id}/revisions", k.httpRevisions)
	h("POST /v1/web/pages/{id}/publish", k.httpPublish)
	h("POST /v1/web/pages/{id}/unschedule", k.httpUnschedule)
	h("POST /v1/web/pages/{id}/archive", k.httpArchive)

	h("POST /v1/web/webhooks", k.httpCreateWebhook)
	h("GET /v1/web/webhooks", k.httpListWebhooks)
	h("DELETE /v1/web/webhooks/{id}", k.httpDisableWebhook)
	h("GET /v1/web/webhooks/{id}/deliveries", k.httpDeliveries)

	if k.Billing != nil {
		h("POST /v1/web/orders", k.httpCreateOrder)
		h("GET /v1/web/orders", k.httpListOrders)
		h("GET /v1/web/orders/{id}", k.httpGetOrder)
		h("GET /v1/web/orders/{id}/events", k.httpOrderEvents)
		h("POST /v1/web/orders/{id}/pay", k.transitionHandler(k.Pay))
		h("POST /v1/web/orders/{id}/fulfill", k.transitionHandler(k.Fulfill))
		h("POST /v1/web/orders/{id}/cancel", k.transitionHandler(k.Cancel))
		h("POST /v1/web/orders/{id}/refund", k.transitionHandler(k.Refund))
	}

	g("GET /v1/web/public/{site}/pages/{slug...}", k.httpPublicPage)
	g("GET /v1/web/public/{site}/sitemap.xml", k.httpSitemap)
	g("GET /v1/web/public/{site}/robots.txt", k.httpRobots)
}

// adminHandler authenticates with an API key (Authorization: Bearer) or an
// editor web session (cookie; unsafe methods need the CSRF header).
func (k *Kit) adminHandler(fn func(w http.ResponseWriter, r *http.Request, a actor) error) http.Handler {
	wrap := func(w http.ResponseWriter, r *http.Request, a actor) error {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		err := fn(w, r, a)
		if errors.Is(err, store.ErrOverloaded) {
			return httpx.Overloaded("write queue full")
		}
		return err
	}
	byKey := httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		key, err := k.Keys.Verify(r.Context(), httpx.BearerToken(r))
		if err != nil {
			return err
		}
		if !key.Has(Scope) {
			return httpx.Forbidden("api key lacks web scope")
		}
		if strings.TrimSpace(key.TenantID) == "" {
			return httpx.Forbidden("api key has no tenant")
		}
		return wrap(w, r, actor{Tenant: key.TenantID, By: "key:" + key.ID, Key: true})
	})
	var bySession http.Handler
	if k.WebAuth != nil {
		bySession = k.WebAuth.Require(httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			s, ok := webauth.SessionFrom(r.Context())
			if !ok {
				return httpx.Unauthorized("login required")
			}
			var one int
			err := k.DB.Read.QueryRowContext(r.Context(), `SELECT 1 FROM web_editors WHERE tenant_id=? AND user_id=?`, s.TenantID, s.UserID).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return httpx.Forbidden("user is not an editor of this tenant")
			}
			if err != nil {
				return err
			}
			return wrap(w, r, actor{Tenant: s.TenantID, By: "user:" + s.UserID})
		}))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bySession == nil || httpx.BearerToken(r) != "" {
			byKey.ServeHTTP(w, r)
			return
		}
		bySession.ServeHTTP(w, r)
	})
}

func (k *Kit) httpAddEditor(w http.ResponseWriter, r *http.Request, a actor) error {
	if !a.Key {
		return httpx.Forbidden("editors are managed with an API key")
	}
	var req struct {
		UserID string `json:"user_id"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.UserID == "" || len(req.UserID) > 64 {
		return httpx.BadRequest("user_id required")
	}
	ctx := r.Context()
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO web_editors(tenant_id,user_id,created_at) VALUES(?,?,?) ON CONFLICT(tenant_id,user_id) DO NOTHING`,
			a.Tenant, req.UserID, k.now().UnixMilli())
		return err
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"tenant_id": a.Tenant, "user_id": req.UserID})
	return nil
}

func (k *Kit) httpRemoveEditor(w http.ResponseWriter, r *http.Request, a actor) error {
	if !a.Key {
		return httpx.Forbidden("editors are managed with an API key")
	}
	ctx := r.Context()
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM web_editors WHERE tenant_id=? AND user_id=?`, a.Tenant, r.PathValue("user"))
		return err
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
