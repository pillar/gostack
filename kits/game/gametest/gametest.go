// Package gametest is a test harness for GameKit feature packages: it runs a
// real app (SQLite in a temp dir, migrations, GameKit) behind httptest with
// two games ("alpha", "beta") and helpers for players and admin keys.
package gametest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
)

// Options configures an Env.
type Options struct {
	// Config is the GameKit document published to every game ("{}" when empty).
	Config string
	// Routes registers the feature package's routes.
	Routes func(mux *http.ServeMux, a *app.App)
	// Mutate adjusts the application config before start.
	Mutate func(*config.Config)
}

// Env is a running test server.
type Env struct {
	T   *testing.T
	App *app.App
	URL string
	// AdminToken is an admin-scoped API key for tenant "default".
	AdminToken string
}

// New starts an Env; it is closed by t.Cleanup.
func New(t *testing.T, o Options) *Env {
	t.Helper()
	cfg := config.Defaults()
	dir := t.TempDir()
	cfg.Database.Path = filepath.Join(dir, "app.db")
	cfg.Database.AnalyticsPath = filepath.Join(dir, "analytics.db")
	cfg.Kits = []string{"game"}
	cfg.Game.AuthRatePerMinute = 10000
	cfg.Game.ScoreRatePerMinute = 10000
	if o.Mutate != nil {
		o.Mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	a, err := app.New(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	doc := o.Config
	if doc == "" {
		doc = "{}"
	}
	for _, g := range []string{"alpha", "beta"} {
		if err := game.CreateGame(ctx, a.DB, "default", g, g, []string{"https://" + g + ".example"}); err != nil {
			t.Fatal(err)
		}
		if _, err := game.PublishConfig(ctx, a.DB, g, []byte(doc), "test", ""); err != nil {
			t.Fatal(err)
		}
	}
	_, tok, err := (auth.APIKeys{DB: a.DB}).Create(ctx, "default", "test", []string{"admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler(app.Hooks{Routes: o.Routes}))
	bg, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { a.Game.Analytics.Run(bg, time.Second); close(done) }()
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
		a.Close()
	})
	return &Env{T: t, App: a, URL: srv.URL, AdminToken: tok}
}

// Resp is a decoded HTTP response.
type Resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

// Code returns the error code of an error envelope ("" otherwise).
func (r Resp) Code() string {
	if e, ok := r.Body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

// Do sends a request. body may be nil, a string (sent verbatim) or a value (JSON).
// hdr is a list of header name/value pairs.
func (e *Env) Do(method, path, token string, body any, hdr ...string) Resp {
	e.T.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewBufferString(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := Resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
	_ = json.Unmarshal(raw, &r.Body)
	return r
}

// Expect fails the test unless r has the given status.
func (e *Env) Expect(r Resp, status int) Resp {
	e.T.Helper()
	if r.Status != status {
		e.T.Fatalf("status %d want %d: %s", r.Status, status, r.Raw)
	}
	return r
}

// Player is a guest player with a session token.
type Player struct{ ID, Token string }

// Guest registers a new guest player in gameID.
func (e *Env) Guest(gameID string) Player {
	e.T.Helper()
	r := e.Expect(e.Do("POST", "/v1/games/"+gameID+"/auth/guest", "", "{}"), 201)
	return Player{ID: r.Body["player_id"].(string), Token: r.Body["access_token"].(string)}
}
