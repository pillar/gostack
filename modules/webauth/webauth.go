// Package webauth provides opt-in email/password authentication with cookie
// sessions for browser applications (PRD F05, §5.2).
//
// Security properties:
//   - Passwords: Argon2id PHC strings (see password.go), 8..128 bytes; unknown
//     accounts are still hashed against a dummy so timing does not reveal them,
//     and every bad-credential case returns the same 401 invalid_credentials.
//   - Sessions: 256-bit opaque token in an HttpOnly, SameSite=Lax cookie; only
//     its SHA-256 is stored. In production the cookie is Secure and named with
//     the __Host- prefix (host-only, Path=/). Absolute TTL plus idle expiry.
//   - CSRF: unsafe methods need the per-session X-CSRF-Token header (returned by
//     login/signup/me) and pass an Origin / Sec-Fetch-Site check; login and
//     signup get the origin check too (login CSRF). JSON bodies are required.
//   - Abuse: password checks are rate limited per IP and per account
//     (process-local) and concurrent Argon2id work is bounded.
//   - Password change revokes every session of the user and rotates the caller's.
//
// Tenant: users are created in Module.Tenant; the session is the only source of
// tenant and user identity for protected routes (see Require / SessionFrom).
package webauth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/ratelimit"
)

// Module is the web auth feature. Set the fields, call Init, then Routes.
type Module struct {
	DB  *store.DB
	Cfg config.WebAuthConfig
	// Secure enables the Secure flag and the __Host- cookie prefix. The app sets
	// it for environment=production; HTTPS is then mandatory for browsers.
	Secure   bool
	ClientIP *httpx.ClientIP
	// Tenant receives new signups (default "default").
	Tenant string
	Now    func() time.Time
	Log    *slog.Logger
	// Limits builds the password-check limiters (nil: process-local).
	Limits ratelimit.Factory

	ipLimit   ratelimit.Allower
	acctLimit ratelimit.Allower
	hashSlots chan struct{}
	dummyHash string
}

// Session is the authenticated context of a request.
type Session struct {
	ID        string `json:"-"`
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	CSRFToken string `json:"-"`
	ExpiresAt int64  `json:"expires_at"`
}

type ctxKey struct{}

// SessionFrom returns the session attached by Require.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(Session)
	return s, ok
}

const (
	cookieBase    = "gostack_session"
	csrfHeader    = "X-CSRF-Token"
	tokenPrefix   = "gws_"
	maxEmailBytes = 254
)

// Init prepares limiters and the dummy hash. Call once before serving.
func (m *Module) Init() {
	if m.Now == nil {
		m.Now = time.Now
	}
	if m.Tenant == "" {
		m.Tenant = "default"
	}
	if m.Log == nil {
		m.Log = slog.Default()
	}
	if m.ClientIP == nil {
		m.ClientIP, _ = httpx.NewClientIP(nil)
	}
	// A global limiter whose backend fails degrades to per-instance buckets:
	// password guessing stays bounded and login stays available.
	f := ratelimit.OrLocal(m.Limits)
	m.ipLimit = f("web_auth_ip", m.Cfg.IPRatePerMinute, ratelimit.FailLocal)
	m.acctLimit = f("web_auth_account", m.Cfg.AccountRatePerMinute, ratelimit.FailLocal)
	m.hashSlots = make(chan struct{}, max(m.Cfg.MaxConcurrentHashes, 1))
	m.dummyHash = hashPassword(auth.NewSecret("dummy_"))
}

// SetRates retunes the password-check limiters (config hot reload).
func (m *Module) SetRates(ipPerMinute, accountPerMinute int) {
	m.ipLimit.SetPerMinute(ipPerMinute)
	m.acctLimit.SetPerMinute(accountPerMinute)
}

// CookieName is the session cookie name ("__Host-" prefixed when Secure).
func (m *Module) CookieName() string {
	if m.Secure {
		return "__Host-" + cookieBase
	}
	return cookieBase
}

// Routes registers /v1/auth/*.
func (m *Module) Routes(mux *http.ServeMux) {
	h := func(f httpx.HandlerFunc) http.Handler { return m.CORS(f) }
	mux.Handle("POST /v1/auth/signup", h(m.signup))
	mux.Handle("POST /v1/auth/login", h(m.login))
	mux.Handle("POST /v1/auth/logout", h(m.required(m.logout)))
	mux.Handle("GET /v1/auth/me", h(m.required(m.me)))
	mux.Handle("POST /v1/auth/password", h(m.required(m.changePassword)))
	mux.Handle("OPTIONS /v1/auth/", h(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
}

// Require protects an application handler: it resolves the session cookie
// (401 when missing/expired/revoked) and, for unsafe methods, enforces the
// origin check and the X-CSRF-Token header (403). The session is available
// via SessionFrom.
func (m *Module) Require(next http.Handler) http.Handler {
	return m.required(func(w http.ResponseWriter, r *http.Request) error {
		next.ServeHTTP(w, r)
		return nil
	})
}

// CORS adds credentialed CORS headers for origins listed in
// auth.web.allowed_origins and answers their preflight requests. Same-origin
// deployments do not need it.
func (m *Module) CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && slices.Contains(m.Cfg.AllowedOrigins, o) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", o)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
				h.Set("Access-Control-Allow-Headers", "Content-Type, "+csrfHeader)
				h.Set("Access-Control-Max-Age", "600")
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Module) required(next httpx.HandlerFunc) httpx.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		s, err := m.resolve(r)
		if err != nil {
			return err
		}
		if !safeMethod(r.Method) {
			if err := m.checkOrigin(r); err != nil {
				return err
			}
			got := r.Header.Get(csrfHeader)
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.CSRFToken)) != 1 {
				return httpx.NewError(http.StatusForbidden, "csrf_failed", "missing or invalid "+csrfHeader)
			}
		}
		return next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
	}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// checkOrigin rejects cross-site browser requests. Requests without Origin and
// Sec-Fetch-Site (non-browser clients) pass; cookie-authenticated ones still
// need the CSRF token, which a cross-site page cannot read.
func (m *Module) checkOrigin(r *http.Request) error {
	deny := httpx.NewError(http.StatusForbidden, "csrf_failed", "cross-origin request rejected")
	origin := r.Header.Get("Origin")
	allowed := origin != "" && slices.Contains(m.Cfg.AllowedOrigins, origin)
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default: // same-site, cross-site
		if !allowed {
			return deny
		}
	}
	if origin == "" || allowed {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) || (m.Secure && u.Scheme != "https") {
		return deny
	}
	return nil
}

func (m *Module) resolve(r *http.Request) (Session, error) {
	c, err := r.Cookie(m.CookieName())
	if err != nil || !strings.HasPrefix(c.Value, tokenPrefix) {
		return Session{}, httpx.Unauthorized("login required")
	}
	ctx := r.Context()
	var s Session
	var lastSeen int64
	err = m.DB.Read.QueryRowContext(ctx, `
		SELECT s.id, s.tenant_id, s.user_id, u.email, s.csrf_token, s.expires_at, s.last_seen_at
		FROM web_sessions s JOIN web_users u ON u.tenant_id=s.tenant_id AND u.id=s.user_id
		WHERE s.token_hash=?`, auth.Hash(c.Value)).
		Scan(&s.ID, &s.TenantID, &s.UserID, &s.Email, &s.CSRFToken, &s.ExpiresAt, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, httpx.Unauthorized("session expired or revoked")
	}
	if err != nil {
		return Session{}, err
	}
	now := m.Now().UnixMilli()
	idle := m.Cfg.IdleTimeout.Milliseconds()
	if s.ExpiresAt <= now || lastSeen+idle <= now {
		return Session{}, httpx.Unauthorized("session expired or revoked")
	}
	// Refresh the idle clock at most every idle/10 (bounded writes per session).
	if now-lastSeen >= idle/10 {
		if err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE web_sessions SET last_seen_at=? WHERE id=? AND last_seen_at<?`, now, s.ID, now)
			return err
		}); err != nil {
			m.Log.Warn("webauth: session touch failed", "err", err)
		}
	}
	return s, nil
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userJSON struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	Email     string `json:"email"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

type sessionResponse struct {
	User      userJSON `json:"user"`
	CSRFToken string   `json:"csrf_token"`
	ExpiresAt int64    `json:"expires_at"`
}

func requireJSON(r *http.Request) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return httpx.NewError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
	}
	return nil
}

// normalizeEmail trims and lowercases; it accepts one '@' with non-empty
// local and domain parts and no whitespace/control characters.
func normalizeEmail(s string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(s))
	if len(e) < 3 || len(e) > maxEmailBytes || strings.Count(e, "@") != 1 {
		return "", false
	}
	local, domain, _ := strings.Cut(e, "@")
	if local == "" || domain == "" || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", false
	}
	for _, r := range e {
		if r <= ' ' || r == 0x7f {
			return "", false
		}
	}
	return e, true
}

func (m *Module) limitIP(r *http.Request) error {
	if ok, retry := m.ipLimit.Allow(ratelimit.IPKey(m.ClientIP.Of(r))); !ok {
		return httpx.RateLimited(retry)
	}
	return nil
}

func (m *Module) limitAccount(tenant, email string) error {
	if ok, retry := m.acctLimit.Allow(tenant + "\x00" + email); !ok {
		return httpx.RateLimited(retry)
	}
	return nil
}

// withHashSlot bounds concurrent Argon2id work; waiting is bounded by the
// request deadline.
func (m *Module) withHashSlot(ctx context.Context, f func()) error {
	select {
	case m.hashSlots <- struct{}{}:
	case <-ctx.Done():
		return httpx.Overloaded("password hashing busy")
	}
	defer func() { <-m.hashSlots }()
	f()
	return nil
}

func invalidCredentials() error {
	return httpx.NewError(http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
}

func (m *Module) signup(w http.ResponseWriter, r *http.Request) error {
	if !m.Cfg.AllowSignup {
		return httpx.NewError(http.StatusForbidden, "signup_disabled", "signup is disabled")
	}
	if err := m.preamble(r); err != nil {
		return err
	}
	var in credentials
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	email, ok := normalizeEmail(in.Email)
	if !ok {
		return httpx.NewError(http.StatusBadRequest, "invalid_email", "invalid email address")
	}
	if !validPassword(in.Password) {
		return httpx.NewError(http.StatusBadRequest, "invalid_password", "password must be 8..128 bytes")
	}
	var phc string
	if err := m.withHashSlot(r.Context(), func() { phc = hashPassword(in.Password) }); err != nil {
		return err
	}
	u := userJSON{ID: httpx.RandomID("usr_"), TenantID: m.Tenant, Email: email, CreatedAt: m.Now().UnixMilli()}
	ns := m.newSession(u.TenantID, u.ID)
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO web_users(id,tenant_id,email,password_hash,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			u.ID, u.TenantID, u.Email, phc, u.CreatedAt, u.CreatedAt); err != nil {
			return err
		}
		return ns.insert(r.Context(), tx)
	})
	if store.IsUniqueViolation(err) {
		return httpx.Conflict("email_taken", "an account with this email already exists")
	}
	if err != nil {
		return mapWriteErr(err)
	}
	m.setCookie(w, ns.token)
	httpx.WriteJSON(w, http.StatusCreated, sessionResponse{User: u, CSRFToken: ns.csrf, ExpiresAt: ns.expires})
	return nil
}

// preamble applies the checks shared by unauthenticated mutations.
func (m *Module) preamble(r *http.Request) error {
	if err := m.checkOrigin(r); err != nil {
		return err
	}
	if err := requireJSON(r); err != nil {
		return err
	}
	return m.limitIP(r)
}

func (m *Module) login(w http.ResponseWriter, r *http.Request) error {
	if err := m.preamble(r); err != nil {
		return err
	}
	var in credentials
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	email, okEmail := normalizeEmail(in.Email)
	if okEmail {
		if err := m.limitAccount(m.Tenant, email); err != nil {
			return err
		}
	}
	var u userJSON
	phc := m.dummyHash
	found := false
	if okEmail && validPassword(in.Password) {
		err := m.DB.Read.QueryRowContext(r.Context(), `SELECT id,tenant_id,email,created_at,password_hash FROM web_users WHERE tenant_id=? AND email=?`,
			m.Tenant, email).Scan(&u.ID, &u.TenantID, &u.Email, &u.CreatedAt, &phc)
		switch {
		case err == nil:
			found = true
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	// Always run one Argon2id verification so unknown accounts and malformed
	// input cost the same as a wrong password.
	pw := in.Password
	if len(pw) > MaxPasswordBytes {
		pw = pw[:MaxPasswordBytes] // bound hashing input; the attempt fails anyway
	}
	var match bool
	var verr error
	if err := m.withHashSlot(r.Context(), func() { match, verr = verifyPassword(pw, phc) }); err != nil {
		return err
	}
	if verr != nil && found {
		return verr
	}
	if !found || !match {
		return invalidCredentials()
	}
	ns := m.newSession(u.TenantID, u.ID)
	old := ""
	if c, err := r.Cookie(m.CookieName()); err == nil {
		old = auth.Hash(c.Value) // session fixation: drop any session the browser carried in
	}
	if err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if old != "" {
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM web_sessions WHERE token_hash=?`, old); err != nil {
				return err
			}
		}
		return ns.insert(r.Context(), tx)
	}); err != nil {
		return mapWriteErr(err)
	}
	m.setCookie(w, ns.token)
	httpx.WriteJSON(w, http.StatusOK, sessionResponse{User: u, CSRFToken: ns.csrf, ExpiresAt: ns.expires})
	return nil
}

func (m *Module) logout(w http.ResponseWriter, r *http.Request) error {
	s, _ := SessionFrom(r.Context())
	if err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), `DELETE FROM web_sessions WHERE id=?`, s.ID)
		return err
	}); err != nil {
		return mapWriteErr(err)
	}
	m.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (m *Module) me(w http.ResponseWriter, r *http.Request) error {
	s, _ := SessionFrom(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, sessionResponse{
		User: userJSON{ID: s.UserID, TenantID: s.TenantID, Email: s.Email}, CSRFToken: s.CSRFToken, ExpiresAt: s.ExpiresAt})
	return nil
}

func (m *Module) changePassword(w http.ResponseWriter, r *http.Request) error {
	if err := requireJSON(r); err != nil {
		return err
	}
	s, _ := SessionFrom(r.Context())
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if !validPassword(in.New) {
		return httpx.NewError(http.StatusBadRequest, "invalid_password", "password must be 8..128 bytes")
	}
	if err := m.limitAccount(s.TenantID, s.Email); err != nil {
		return err
	}
	var phc string
	if err := m.DB.Read.QueryRowContext(r.Context(), `SELECT password_hash FROM web_users WHERE tenant_id=? AND id=?`, s.TenantID, s.UserID).Scan(&phc); err != nil {
		return err
	}
	var match bool
	var verr error
	var next string
	if err := m.withHashSlot(r.Context(), func() {
		if match, verr = verifyPassword(in.Current, phc); match {
			next = hashPassword(in.New)
		}
	}); err != nil {
		return err
	}
	if verr != nil {
		return verr
	}
	if !match {
		return invalidCredentials()
	}
	ns := m.newSession(s.TenantID, s.UserID)
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		// Conditional on the hash we verified: a concurrent change wins once.
		res, err := tx.ExecContext(r.Context(), `UPDATE web_users SET password_hash=?, updated_at=? WHERE tenant_id=? AND id=? AND password_hash=?`,
			next, m.Now().UnixMilli(), s.TenantID, s.UserID, phc)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return httpx.Conflict("password_changed", "password was changed concurrently; log in again")
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM web_sessions WHERE tenant_id=? AND user_id=?`, s.TenantID, s.UserID); err != nil {
			return err
		}
		return ns.insert(r.Context(), tx)
	})
	if err != nil {
		return mapWriteErr(err)
	}
	m.setCookie(w, ns.token)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"csrf_token": ns.csrf, "expires_at": ns.expires})
	return nil
}

type newSession struct {
	id, tenant, user, token, csrf string
	now, expires                  int64
}

func (m *Module) newSession(tenant, user string) newSession {
	now := m.Now().UnixMilli()
	return newSession{id: httpx.RandomID("wss_"), tenant: tenant, user: user, token: auth.NewSecret(tokenPrefix),
		csrf: auth.NewSecret(""), now: now, expires: now + m.Cfg.SessionTTL.Milliseconds()}
}

func (s newSession) insert(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO web_sessions(id,tenant_id,user_id,token_hash,csrf_token,created_at,last_seen_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		s.id, s.tenant, s.user, auth.Hash(s.token), s.csrf, s.now, s.now, s.expires)
	return err
}

func (m *Module) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: m.CookieName(), Value: token, Path: "/", MaxAge: int(m.Cfg.SessionTTL.Seconds()),
		HttpOnly: true, Secure: m.Secure, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
}

func (m *Module) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: m.CookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: m.Secure, SameSite: http.SameSiteLaxMode})
}

func mapWriteErr(err error) error {
	if errors.Is(err, store.ErrOverloaded) {
		return httpx.Overloaded("write queue full")
	}
	return err
}

// Maintain deletes sessions past their absolute TTL or idle timeout.
func (m *Module) Maintain(ctx context.Context) error {
	now := m.Now().UnixMilli()
	return m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM web_sessions WHERE expires_at<=? OR last_seen_at<=?`,
			now, now-m.Cfg.IdleTimeout.Milliseconds())
		return err
	})
}
