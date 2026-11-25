package webauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/webauth"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	m   *webauth.Module
	db  *store.DB
	now atomic.Int64 // unix ms
}

func setup(t *testing.T, mut func(*webauth.Module)) *env {
	t.Helper()
	o := sqlite.Options{Path: filepath.Join(t.TempDir(), "app.db"), ReadConnections: 2, Synchronous: "FULL",
		BusyTimeout: time.Second, WALAutocheckpoint: 1000, WriteQueue: 64}
	db := pgtest.Open(t, o)
	e := &env{t: t, db: db}
	e.now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	cfg := config.Defaults().Auth.Web
	cfg.Enabled = true
	cfg.IPRatePerMinute, cfg.AccountRatePerMinute = 1000, 1000
	ip, _ := httpx.NewClientIP([]string{"127.0.0.0/8"})
	e.m = &webauth.Module{DB: db, Cfg: cfg, ClientIP: ip, Now: func() time.Time { return time.UnixMilli(e.now.Load()) }}
	if mut != nil {
		mut(e.m)
	}
	e.m.Init()
	mux := http.NewServeMux()
	e.m.Routes(mux)
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, _ := webauth.SessionFrom(r.Context())
		httpx.WriteJSON(w, 200, map[string]string{"user_id": s.UserID, "tenant_id": s.TenantID})
	})
	mux.Handle("GET /app/data", e.m.Require(protected))
	mux.Handle("POST /app/data", e.m.Require(protected))
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

type resp struct {
	status int
	body   map[string]any
	cookie *http.Cookie
	header http.Header
}

func (r resp) code() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

// do sends a request. hdr may set "Cookie" (token value), "X-CSRF-Token", Origin etc.
func (e *env) do(method, path string, body any, hdr map[string]string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if k == "Cookie" {
			req.AddCookie(&http.Cookie{Name: e.m.CookieName(), Value: v})
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{status: res.StatusCode, header: res.Header}
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &out.body)
	for _, c := range res.Cookies() {
		if c.Name == e.m.CookieName() {
			out.cookie = c
		}
	}
	return out
}

type sess struct{ token, csrf string }

func (e *env) signup(email, pw string) sess {
	e.t.Helper()
	r := e.do("POST", "/v1/auth/signup", map[string]string{"email": email, "password": pw}, nil)
	if r.status != 201 || r.cookie == nil {
		e.t.Fatalf("signup: %d %v", r.status, r.body)
	}
	return sess{r.cookie.Value, r.body["csrf_token"].(string)}
}

func (e *env) login(email, pw string) sess {
	e.t.Helper()
	r := e.do("POST", "/v1/auth/login", map[string]string{"email": email, "password": pw}, nil)
	if r.status != 200 || r.cookie == nil {
		e.t.Fatalf("login: %d %v", r.status, r.body)
	}
	return sess{r.cookie.Value, r.body["csrf_token"].(string)}
}

func (s sess) hdr(extra ...string) map[string]string {
	h := map[string]string{"Cookie": s.token, "X-CSRF-Token": s.csrf}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

func TestSignupLoginMeLogout(t *testing.T) {
	e := setup(t, nil)
	r := e.do("POST", "/v1/auth/signup", map[string]string{"email": "  Alice@Example.COM ", "password": "correct horse"}, nil)
	if r.status != 201 {
		t.Fatalf("signup %d %v", r.status, r.body)
	}
	c := r.cookie
	if c == nil || c.Name != "gostack_session" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure || c.MaxAge != int((7*24*time.Hour).Seconds()) {
		t.Fatalf("cookie flags: %+v", c)
	}
	if u := r.body["user"].(map[string]any); u["email"] != "alice@example.com" || u["tenant_id"] != "default" {
		t.Fatalf("user %v", u)
	}
	var stored string
	e.db.Read.QueryRow(`SELECT password_hash FROM web_users`).Scan(&stored)
	if !strings.HasPrefix(stored, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash format %q", stored)
	}
	var tokenInDB int
	e.db.Read.QueryRow(`SELECT count(*) FROM web_sessions WHERE token_hash=?`, c.Value).Scan(&tokenInDB)
	if tokenInDB != 0 {
		t.Fatal("plaintext session token stored")
	}
	if r := e.do("POST", "/v1/auth/signup", map[string]string{"email": "alice@example.com", "password": "another pw"}, nil); r.status != 409 || r.code() != "email_taken" {
		t.Fatalf("duplicate signup %d %v", r.status, r.body)
	}
	s := e.login("ALICE@example.com", "correct horse")
	me := e.do("GET", "/v1/auth/me", nil, map[string]string{"Cookie": s.token})
	if me.status != 200 || me.body["csrf_token"] != s.csrf || me.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("me %d %v", me.status, me.body)
	}
	if r := e.do("GET", "/app/data", nil, map[string]string{"Cookie": s.token}); r.status != 200 || r.body["tenant_id"] != "default" {
		t.Fatalf("protected %d %v", r.status, r.body)
	}
	if r := e.do("POST", "/v1/auth/logout", nil, map[string]string{"Cookie": s.token}); r.status != 403 || r.code() != "csrf_failed" {
		t.Fatalf("logout without csrf %d", r.status)
	}
	r = e.do("POST", "/v1/auth/logout", nil, s.hdr())
	if r.status != 204 || r.cookie == nil || r.cookie.MaxAge >= 0 {
		t.Fatalf("logout %d cookie %+v", r.status, r.cookie)
	}
	if r := e.do("GET", "/v1/auth/me", nil, map[string]string{"Cookie": s.token}); r.status != 401 {
		t.Fatalf("revoked session still valid: %d", r.status)
	}
	if r := e.do("GET", "/app/data", nil, nil); r.status != 401 {
		t.Fatalf("no cookie: %d", r.status)
	}
}

func TestSecureCookieAndHTTPSOrigin(t *testing.T) {
	e := setup(t, func(m *webauth.Module) { m.Secure = true })
	r := e.do("POST", "/v1/auth/signup", map[string]string{"email": "b@example.com", "password": "password1"}, nil)
	c := r.cookie
	if r.status != 201 || c == nil || c.Name != "__Host-gostack_session" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" {
		t.Fatalf("production cookie: %d %+v", r.status, c)
	}
	host := strings.TrimPrefix(e.srv.URL, "http://")
	if r := e.do("POST", "/v1/auth/login", map[string]string{"email": "b@example.com", "password": "password1"}, map[string]string{"Origin": "http://" + host}); r.status != 403 {
		t.Fatalf("plain-http origin accepted in secure mode: %d", r.status)
	}
	if r := e.do("POST", "/v1/auth/login", map[string]string{"email": "b@example.com", "password": "password1"}, map[string]string{"Origin": "https://" + host}); r.status != 200 {
		t.Fatalf("https same-host origin rejected: %d %v", r.status, r.body)
	}
}

func TestCSRFAndOrigin(t *testing.T) {
	e := setup(t, func(m *webauth.Module) { m.Cfg.AllowedOrigins = []string{"https://app.example.com"} })
	s := e.signup("c@example.com", "password1")
	host := strings.TrimPrefix(e.srv.URL, "http://")
	cases := []struct {
		name   string
		hdr    map[string]string
		status int
	}{
		{"missing token", map[string]string{"Cookie": s.token}, 403},
		{"wrong token", map[string]string{"Cookie": s.token, "X-CSRF-Token": s.csrf + "x"}, 403},
		{"other session's token", map[string]string{"Cookie": s.token, "X-CSRF-Token": e.login("c@example.com", "password1").csrf}, 403},
		{"cross-origin", s.hdr("Origin", "https://evil.example"), 403},
		{"null origin", s.hdr("Origin", "null"), 403},
		{"sec-fetch cross-site", s.hdr("Sec-Fetch-Site", "cross-site"), 403},
		{"sec-fetch same-site", s.hdr("Sec-Fetch-Site", "same-site"), 403},
		{"same origin", s.hdr("Origin", "http://"+host, "Sec-Fetch-Site", "same-origin"), 200},
		{"allowed origin", s.hdr("Origin", "https://app.example.com", "Sec-Fetch-Site", "same-site"), 200},
		{"no origin (non-browser)", s.hdr(), 200},
	}
	for _, c := range cases {
		r := e.do("POST", "/app/data", map[string]string{}, c.hdr)
		if r.status != c.status {
			t.Errorf("%s: status %d want %d (%v)", c.name, r.status, c.status, r.body)
		}
		if c.status == 403 && r.code() != "csrf_failed" {
			t.Errorf("%s: code %q", c.name, r.code())
		}
	}
	// Safe methods need no token.
	if r := e.do("GET", "/app/data", nil, map[string]string{"Cookie": s.token}); r.status != 200 {
		t.Fatalf("GET %d", r.status)
	}
	// Login CSRF: cross-site login form posts are rejected.
	if r := e.do("POST", "/v1/auth/login", map[string]string{"email": "c@example.com", "password": "password1"},
		map[string]string{"Origin": "https://evil.example"}); r.status != 403 {
		t.Fatalf("cross-origin login %d", r.status)
	}
	// Simple-request bodies (form/text) are not accepted.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/auth/login", strings.NewReader(`{"email":"c@example.com","password":"password1"}`))
	req.Header.Set("Content-Type", "text/plain")
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 415 {
		t.Fatalf("text/plain login %d", res.StatusCode)
	}
	// CORS only for configured origins.
	r := e.do("OPTIONS", "/v1/auth/login", nil, map[string]string{"Origin": "https://app.example.com"})
	if r.header.Get("Access-Control-Allow-Origin") != "https://app.example.com" || r.header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("preflight headers %v", r.header)
	}
	r = e.do("OPTIONS", "/v1/auth/login", nil, map[string]string{"Origin": "https://evil.example"})
	if r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS granted to unlisted origin")
	}
}

func TestBadCredentialsUniform(t *testing.T) {
	e := setup(t, nil)
	e.signup("d@example.com", "password1")
	try := func(email, pw string) (resp, time.Duration) {
		start := time.Now()
		r := e.do("POST", "/v1/auth/login", map[string]string{"email": email, "password": pw}, nil)
		return r, time.Since(start)
	}
	wrong, _ := try("d@example.com", "password2")
	unknown, _ := try("nobody@example.com", "password1")
	short, _ := try("d@example.com", "short")
	for _, r := range []resp{wrong, unknown, short} {
		if r.status != 401 || r.code() != "invalid_credentials" || r.body["error"].(map[string]any)["message"] != "invalid email or password" || r.cookie != nil {
			t.Fatalf("non-uniform failure %d %v", r.status, r.body)
		}
	}
	// Unknown accounts still pay for one Argon2id verification.
	minOf := func(email string) time.Duration {
		best := time.Hour
		for i := 0; i < 3; i++ {
			_, d := try(email, "password9")
			best = min(best, d)
		}
		return best
	}
	known, unk := minOf("d@example.com"), minOf("ghost@example.com")
	if unk*2 < known {
		t.Fatalf("unknown user is much faster (%v vs %v): account enumeration by timing", unk, known)
	}
}

func TestPasswordRules(t *testing.T) {
	e := setup(t, nil)
	for pw, want := range map[string]int{"1234567": 400, strings.Repeat("x", 129): 400, strings.Repeat("x", 128): 201, "12345678": 201} {
		email := "u" + string(rune('a'+len(pw)%26)) + string(rune('a'+len(pw)/26)) + "@example.com"
		if r := e.do("POST", "/v1/auth/signup", map[string]string{"email": email, "password": pw}, nil); r.status != want {
			t.Errorf("password len %d: %d want %d (%v)", len(pw), r.status, want, r.body)
		}
	}
	for _, bad := range []string{"no-at", "a@@b", "@x.y", "a b@c.d", "a@.c"} {
		if r := e.do("POST", "/v1/auth/signup", map[string]string{"email": bad, "password": "password1"}, nil); r.status != 400 || r.code() != "invalid_email" {
			t.Errorf("email %q: %d", bad, r.status)
		}
	}
	e2 := setup(t, func(m *webauth.Module) { m.Cfg.AllowSignup = false })
	if r := e2.do("POST", "/v1/auth/signup", map[string]string{"email": "x@example.com", "password": "password1"}, nil); r.status != 403 || r.code() != "signup_disabled" {
		t.Fatalf("signup disabled: %d", r.status)
	}
}

func TestRateLimits(t *testing.T) {
	e := setup(t, func(m *webauth.Module) { m.Cfg.IPRatePerMinute, m.Cfg.AccountRatePerMinute = 3, 2 })
	login := func(email, ip string) int {
		return e.do("POST", "/v1/auth/login", map[string]string{"email": email, "password": "password1"}, map[string]string{"X-Forwarded-For": ip}).status
	}
	// Per account: third attempt for the same account is limited even from a fresh IP.
	if login("v@example.com", "203.0.113.1") != 401 || login("v@example.com", "203.0.113.2") != 401 {
		t.Fatal("first attempts should be evaluated")
	}
	if s := login("V@Example.com", "203.0.113.3"); s != 429 {
		t.Fatalf("account limit: %d", s)
	}
	// Per IP: fourth attempt from one IP is limited across accounts.
	for i, want := range []int{401, 401, 401, 429} {
		if s := login("acct"+string(rune('a'+i))+"@example.com", "198.51.100.7"); s != want {
			t.Fatalf("ip attempt %d: %d want %d", i, s, want)
		}
	}
	r := e.do("POST", "/v1/auth/login", map[string]string{"email": "z@example.com", "password": "password1"}, map[string]string{"X-Forwarded-For": "198.51.100.7"})
	if r.header.Get("Retry-After") == "" || r.code() != "rate_limited" {
		t.Fatalf("429 without Retry-After: %v", r.header)
	}
	// Hot reload raises the budget for new attempts.
	e.m.SetRates(100, 100)
	if s := login("w@example.com", "192.0.2.1"); s != 401 {
		t.Fatalf("after SetRates: %d", s)
	}
}

func TestPasswordChangeRevokesSessions(t *testing.T) {
	e := setup(t, nil)
	a := e.signup("p@example.com", "old password")
	b := e.login("p@example.com", "old password")
	body := func(cur, nw string) map[string]string {
		return map[string]string{"current_password": cur, "new_password": nw}
	}
	if r := e.do("POST", "/v1/auth/password", body("wrong pass", "new password"), a.hdr()); r.status != 401 || r.code() != "invalid_credentials" {
		t.Fatalf("wrong current: %d", r.status)
	}
	if r := e.do("POST", "/v1/auth/password", body("old password", "short"), a.hdr()); r.status != 400 {
		t.Fatalf("short new: %d", r.status)
	}
	if r := e.do("POST", "/v1/auth/password", body("old password", "new password"), map[string]string{"Cookie": a.token}); r.status != 403 {
		t.Fatalf("no csrf: %d", r.status)
	}
	r := e.do("POST", "/v1/auth/password", body("old password", "new password"), a.hdr())
	if r.status != 200 || r.cookie == nil {
		t.Fatalf("change: %d %v", r.status, r.body)
	}
	for name, tok := range map[string]string{"caller old token": a.token, "other session": b.token} {
		if r := e.do("GET", "/v1/auth/me", nil, map[string]string{"Cookie": tok}); r.status != 401 {
			t.Fatalf("%s still valid after password change: %d", name, r.status)
		}
	}
	if r := e.do("GET", "/v1/auth/me", nil, map[string]string{"Cookie": r.cookie.Value}); r.status != 200 {
		t.Fatalf("rotated session invalid: %d", r.status)
	}
	if r := e.do("POST", "/v1/auth/login", map[string]string{"email": "p@example.com", "password": "old password"}, nil); r.status != 401 {
		t.Fatalf("old password still works: %d", r.status)
	}
	e.login("p@example.com", "new password")
}

func TestSessionExpiry(t *testing.T) {
	e := setup(t, func(m *webauth.Module) { m.Cfg.SessionTTL, m.Cfg.IdleTimeout = 2*time.Hour, 30*time.Minute })
	s := e.signup("x@example.com", "password1")
	step := func(d time.Duration) { e.now.Add(d.Milliseconds()) }
	me := func() int { return e.do("GET", "/v1/auth/me", nil, map[string]string{"Cookie": s.token}).status }
	// Activity every 20 minutes keeps the idle clock fresh until the absolute TTL.
	for i := 0; i < 5; i++ {
		step(20 * time.Minute)
		if st := me(); st != 200 {
			t.Fatalf("active session rejected at step %d: %d", i, st)
		}
	}
	step(21 * time.Minute) // 121 min > absolute TTL
	if me() != 401 {
		t.Fatal("absolute TTL not enforced")
	}
	s = e.login("x@example.com", "password1")
	step(31 * time.Minute)
	if me() != 401 {
		t.Fatal("idle timeout not enforced")
	}
	if err := e.m.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	e.db.Read.QueryRow(`SELECT count(*) FROM web_sessions`).Scan(&n)
	if n != 0 {
		t.Fatalf("maintenance left %d expired sessions", n)
	}
}

// Login replaces any session the browser presented (session fixation).
func TestLoginRotatesPresentedSession(t *testing.T) {
	e := setup(t, nil)
	a := e.signup("f@example.com", "password1")
	r := e.do("POST", "/v1/auth/login", map[string]string{"email": "f@example.com", "password": "password1"}, map[string]string{"Cookie": a.token})
	if r.status != 200 || r.cookie == nil || r.cookie.Value == a.token {
		t.Fatalf("login %d", r.status)
	}
	if e.do("GET", "/v1/auth/me", nil, map[string]string{"Cookie": a.token}).status != 401 {
		t.Fatal("presented session survived login")
	}
}
