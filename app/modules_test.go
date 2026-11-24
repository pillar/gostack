package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
)

func allModulesConfig(t *testing.T) config.Config {
	c := testConfig(t)
	c.Game.Modules = []string{"ops", "experiments", "social", "match", "commerce", "realtime"}
	c.Billing.Enabled = true
	if pgtest.Enabled() {
		// Profile scale rejects the in-process realtime hub and local object storage.
		c.Game.Modules = c.Game.Modules[:5]
		return c
	}
	c.Storage.Driver = "local"
	c.Storage.Local.Root = filepath.Join(t.TempDir(), "objects")
	c.Storage.Local.PublicURL = "/v1/files"
	c.Storage.Local.PresignSecretRef = "env:GOSTACK_TEST_PRESIGN"
	t.Setenv("GOSTACK_TEST_PRESIGN", strings.Repeat("s", 32))
	return c
}

// Every enabled module is mounted by the app; disabled modules are not.
func TestModulesWiredByConfig(t *testing.T) {
	a := newTestApp(t, allModulesConfig(t))
	ctx := context.Background()
	if err := game.CreateGame(ctx, a.DB, "default", "demo", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := game.PublishConfig(ctx, a.DB, "demo", []byte(`{}`), "test", ""); err != nil {
		t.Fatal(err)
	}
	_, billingKey, err := (auth.APIKeys{DB: a.DB}).Create(ctx, "default", "b", []string{"billing"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler(Hooks{}))
	defer srv.Close()
	do := func(method, path, token string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	req, _ := http.NewRequest("POST", srv.URL+"/v1/games/demo/auth/guest", strings.NewReader("{}"))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var guest struct {
		Token string `json:"access_token"`
	}
	_ = json.NewDecoder(res.Body).Decode(&guest)
	res.Body.Close()

	for _, c := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/v1/games/demo/announcements", "", 200},               // ops
		{"GET", "/v1/games/demo/mail", guest.Token, 200},               // ops
		{"GET", "/v1/games/demo/experiments", guest.Token, 200},        // experiments
		{"GET", "/v1/games/demo/friends", guest.Token, 200},            // social
		{"GET", "/v1/games/demo/match/tickets/none", guest.Token, 404}, // match (route exists, ticket unknown)
		{"POST", "/v1/games/demo/iap/orders", guest.Token, 400},        // commerce (route exists; body invalid)
		{"GET", "/v1/billing/accounts/none", billingKey, 404},          // billing
		{"GET", "/v1/files/x", "", 403},                                // local storage presign handler (unsigned → 403)
		{"GET", "/v1/games/demo/inventory", guest.Token, 200},          // core GameKit P0
	} {
		if pgtest.Enabled() && c.path == "/v1/files/x" {
			continue // no local storage under profile scale
		}
		if got := do(c.method, c.path, c.token); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, got, c.want)
		}
	}
	if pgtest.Enabled() {
		if a.Mod.Ops == nil || a.Mod.Match == nil || a.Mod.Billing == nil || a.Mod.Realtime != nil {
			t.Fatal("modules not constructed as configured for profile scale")
		}
	} else if a.Mod.Ops == nil || a.Mod.Realtime == nil || a.Mod.Billing == nil || a.Mod.Storage == nil {
		t.Fatal("modules not constructed")
	}

	// A plain GameKit app does not mount optional modules.
	b := newTestApp(t, testConfig(t))
	srv2 := httptest.NewServer(b.Handler(Hooks{}))
	defer srv2.Close()
	res, err = http.Get(srv2.URL + "/v1/games/demo/announcements")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("ops mounted without game.modules: %d", res.StatusCode)
	}
}

func TestModuleConfigValidation(t *testing.T) {
	c := testConfig(t)
	c.Game.Modules = []string{"ops", "chat"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `"chat" is not implemented`) {
		t.Fatalf("err=%v", err)
	}
	c = testConfig(t)
	c.Game.Commerce.Ads = map[string]config.AdNetworkConfig{"x": {Kind: "unity"}}
	if err := c.Validate(); err == nil {
		t.Fatal("unimplemented ad network accepted")
	}
	c = testConfig(t)
	c.Storage.Driver = "gcs"
	if err := c.Validate(); err == nil {
		t.Fatal("unimplemented storage driver accepted")
	}
}

// Serve closes realtime connections (1001) on shutdown and runs module loops.
func TestServeWithModulesShutsDownRealtime(t *testing.T) {
	pgtest.SkipIfPostgres(t, "the realtime module is rejected under profile scale (in-process hub)")
	c := allModulesConfig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP.Listen = ln.Addr().String()
	ln.Close()
	a := newTestApp(t, c)
	ctx := context.Background()
	if err := game.CreateGame(ctx, a.DB, "default", "demo", "demo", nil); err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Serve(sctx, Hooks{}) }()
	base := "http://" + c.HTTP.Listen
	var guest struct {
		Token string `json:"access_token"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := http.Post(base+"/v1/games/demo/auth/guest", "application/json", strings.NewReader("{}"))
		if err == nil {
			_ = json.NewDecoder(res.Body).Decode(&guest)
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wctx, "ws://"+c.HTTP.Listen+"/v1/games/demo/realtime",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + guest.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(wctx); err != nil { // welcome
		t.Fatal(err)
	}
	stop()
	_, _, err = conn.Read(wctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status %v (%v)", websocket.CloseStatus(err), err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// Hot reload retunes GameKit limiters (game.auth_rate_per_minute).
func TestReloadRetunesGameLimiters(t *testing.T) {
	c := testConfig(t)
	a := newTestApp(t, c)
	if err := game.CreateGame(context.Background(), a.DB, "default", "demo", "demo", nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler(Hooks{}))
	defer srv.Close()
	login := func() int {
		res, err := http.Post(srv.URL+"/v1/games/demo/auth/guest", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if login() != 201 || login() != 201 {
		t.Fatal("default rate rejected logins")
	}
	next := a.Config()
	next.Game.AuthRatePerMinute = 1
	if _, err := a.Reload(next); err != nil {
		t.Fatal(err)
	}
	// The existing bucket is truncated to the new burst (1): one more passes, then 429.
	if first, second := login(), login(); first != 201 || second != 429 {
		t.Fatalf("after lowering auth rate: %d, %d; want 201, 429", first, second)
	}
}
