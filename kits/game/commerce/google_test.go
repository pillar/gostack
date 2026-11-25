package commerce_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/commerce"
	"github.com/pillar/gostack/kits/game/gametest"
)

// Everything below runs against an httptest fake of Google's OAuth2 token
// endpoint, the Play Developer API and the OIDC JWKS, with locally generated
// RSA keys. No Google endpoint is contacted.

const (
	gPkg      = "com.alpha.game"
	gAudience = "https://api.example.com/v1/games/alpha/callbacks/google"
	gPushSA   = "rtdn-push@example-project.iam.gserviceaccount.com"
)

type googleFake struct {
	t         *testing.T
	srv       *httptest.Server
	saKey     *rsa.PrivateKey
	oidcKey   *rsa.PrivateKey
	mu        sync.Mutex
	purchases map[string]map[string]any // by token
	tokens    int
	gets      int
	finished  []string // "consume:<token>" / "acknowledge:<token>"
	failGet   bool
	finishErr int // non-zero: consume/acknowledge answer this HTTP status
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims any) string {
	t.Helper()
	hb, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	cb, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func verifyRS256(tok string, pub *rsa.PublicKey) (map[string]any, bool) {
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		return nil, false
	}
	sig, _ := base64.RawURLEncoding.DecodeString(p[2])
	sum := sha256.Sum256([]byte(p[0] + "." + p[1]))
	if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
		return nil, false
	}
	b, _ := base64.RawURLEncoding.DecodeString(p[1])
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	return c, true
}

func newGoogleFake(t *testing.T) *googleFake {
	g := &googleFake{t: t, purchases: map[string]map[string]any{}}
	g.saKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	g.oidcKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *googleFake) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.URL.Path == "/token":
		_ = r.ParseForm()
		c, ok := verifyRS256(r.PostForm.Get("assertion"), &g.saKey.PublicKey)
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || !ok ||
			c["aud"] != g.srv.URL+"/token" || c["scope"] != "https://www.googleapis.com/auth/androidpublisher" || c["iss"] != "svc@example-project.iam.gserviceaccount.com" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		g.tokens++
		w.Write([]byte(`{"access_token":"at-1","expires_in":3600,"token_type":"Bearer"}`))
	case r.URL.Path == "/certs":
		n := base64.RawURLEncoding.EncodeToString(g.oidcKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(g.oidcKey.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "alg": "RS256", "n": n, "e": e}}})
	case strings.HasPrefix(r.URL.Path, "/androidpublisher/v3/applications/"):
		if r.Header.Get("Authorization") != "Bearer at-1" {
			w.WriteHeader(401)
			return
		}
		// /androidpublisher/v3/applications/{pkg}/purchases/products/{product}/tokens/{token}[:action]
		parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/androidpublisher/v3/applications/"), "/")
		if len(parts) != 6 {
			w.WriteHeader(404)
			return
		}
		pkg, _ := url.PathUnescape(parts[0])
		product, _ := url.PathUnescape(parts[3])
		last, _ := url.PathUnescape(parts[5])
		token, action, _ := strings.Cut(last, ":")
		p, ok := g.purchases[token]
		if !ok || pkg != p["_pkg"] || p["productId"] != product {
			w.WriteHeader(404)
			return
		}
		if r.Method == http.MethodPost {
			if g.finishErr != 0 {
				w.WriteHeader(g.finishErr)
				return
			}
			g.finished = append(g.finished, action+":"+token)
			if action == "consume" {
				p["consumptionState"] = 1
			}
			p["acknowledgementState"] = 1
			w.WriteHeader(204)
			return
		}
		if g.failGet {
			w.WriteHeader(503)
			return
		}
		g.gets++
		json.NewEncoder(w).Encode(p)
	default:
		w.WriteHeader(404)
	}
}

// purchase registers a purchase in the fake Play API.
func (g *googleFake) purchase(token, product, account string, mut ...func(map[string]any)) {
	p := map[string]any{"kind": "androidpublisher#productPurchase", "purchaseTimeMillis": "1700000000000", "purchaseState": 0,
		"consumptionState": 0, "acknowledgementState": 0, "orderId": "GPA.0000-" + token, "productId": product, "quantity": 1,
		"obfuscatedExternalAccountId": account, "regionCode": "US", "_pkg": gPkg}
	for _, m := range mut {
		m(p)
	}
	g.mu.Lock()
	g.purchases[token] = p
	g.mu.Unlock()
}

func (g *googleFake) pushToken(t *testing.T, mut ...func(map[string]any)) string {
	c := map[string]any{"iss": "https://accounts.google.com", "aud": gAudience, "azp": "1234", "email": gPushSA, "email_verified": true,
		"sub": "1234", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for _, m := range mut {
		m(c)
	}
	return signRS256(t, g.oidcKey, "k1", c)
}

func pushBody(messageID string, dn map[string]any) map[string]any {
	b, _ := json.Marshal(dn)
	return map[string]any{"message": map[string]any{"data": base64.StdEncoding.EncodeToString(b), "messageId": messageID,
		"publishTime": "2026-11-25T00:00:00Z"}, "subscription": "projects/p/subscriptions/s"}
}

func voided(token string, refundType int) map[string]any {
	return map[string]any{"version": "1.0", "packageName": gPkg, "eventTimeMillis": "1700000000000",
		"voidedPurchaseNotification": map[string]any{"purchaseToken": token, "orderId": "GPA.0000-" + token, "productType": 2, "refundType": refundType}}
}

type gfixture struct {
	*gametest.Env
	g   *googleFake
	mod *commerce.Module
}

// setupGoogle wires the provider through configuration and app wiring.
func setupGoogle(t *testing.T) *gfixture {
	g := newGoogleFake(t)
	der, _ := x509.MarshalPKCS8PrivateKey(g.saKey)
	keyJSON, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "svc@example-project.iam.gserviceaccount.com",
		"private_key_id": "kid-1", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri": "https://oauth2.googleapis.com/token"})
	keyFile := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(keyFile, keyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &gfixture{g: g}
	f.Env = gametest.New(t, gametest.Options{Mutate: func(c *config.Config) {
		c.Game.Modules = []string{"commerce"}
		c.Game.Commerce.Google = &config.GoogleIAPConfig{ServiceAccountRef: "file:" + keyFile, TokenURL: g.srv.URL + "/token", APIBaseURL: g.srv.URL,
			Apps: map[string]config.GoogleAppConf{"alpha": {PackageName: gPkg}, "beta": {PackageName: "com.beta.game", Finish: "acknowledge"}},
			RTDN: &config.GoogleRTDNConf{Audience: gAudience, ServiceAccountEmail: gPushSA, JWKSURL: g.srv.URL + "/certs"}}
	}})
	f.mod = f.App.Mod.Commerce
	for _, gm := range []string{"alpha", "beta"} {
		f.Expect(f.Do("PUT", "/v1/admin/games/"+gm+"/products", f.AdminToken, map[string]any{
			"gems100": map[string]any{"grants": map[string]int{"gems": 100}, "provider_product_ids": map[string]string{"google": "gems_100"}},
			"gems500": map[string]any{"grants": map[string]int{"gems": 500}, "provider_product_ids": map[string]string{"google": "gems_500"}},
		}), 200)
	}
	return f
}

func (f *gfixture) order(gameID string, p gametest.Player) string {
	f.T.Helper()
	r := f.Expect(f.Do("POST", "/v1/games/"+gameID+"/iap/orders", p.Token, map[string]string{"product_id": "gems100"}), 201)
	return r.Body["app_account_token"].(string)
}

func (f *gfixture) verify(gameID string, p gametest.Player, product, token string) gametest.Resp {
	return f.Do("POST", "/v1/games/"+gameID+"/iap/google/verify", p.Token, map[string]string{"product_id": product, "purchase_token": token})
}

func (f *gfixture) push(t *testing.T, gameID, messageID string, dn map[string]any) gametest.Resp {
	return f.Do("POST", "/v1/games/"+gameID+"/callbacks/google", f.g.pushToken(t), pushBody(messageID, dn))
}

func (f *gfixture) gems(gameID string, p gametest.Player) int64 {
	f.T.Helper()
	r := f.Expect(f.Do("GET", "/v1/games/"+gameID+"/wallet", p.Token, nil), 200)
	n, _ := r.Body["balances"].(map[string]any)["gems"].(float64)
	return int64(n)
}

// runFinishJobs runs the queued Google acknowledge/consume jobs.
func (f *gfixture) runFinishJobs() int {
	f.T.Helper()
	ctx := context.Background()
	h := f.mod.JobHandlers()[commerce.JobGoogleFinish]
	n := 0
	for {
		c, err := f.App.Jobs.Claim(ctx, []string{commerce.JobGoogleFinish}, time.Minute)
		if err != nil {
			f.T.Fatal(err)
		}
		if c == nil {
			return n
		}
		if err := h(ctx, c); err != nil {
			f.T.Fatal(err)
		}
		if err := f.App.Jobs.Ack(ctx, c); err != nil {
			f.T.Fatal(err)
		}
		n++
	}
}

func TestGoogleVerifyDeliversOnceAndConsumes(t *testing.T) {
	f := setupGoogle(t)
	p := f.Guest("alpha")
	f.g.purchase("tok-1", "gems_100", f.order("alpha", p))
	r := f.Expect(f.verify("alpha", p, "gems_100", "tok-1"), 200)
	o := r.Body["order"].(map[string]any)
	if o["state"] != "delivered" || r.Body["replayed"] != false || o["product_id"] != "gems100" || f.gems("alpha", p) != 100 {
		t.Fatalf("%s", r.Raw)
	}
	if r := f.Expect(f.verify("alpha", p, "gems_100", "tok-1"), 200); r.Body["replayed"] != true {
		t.Fatalf("%s", r.Raw)
	}
	// The consume call is a job committed with the delivery, run after it.
	if n := f.runFinishJobs(); n != 1 {
		t.Fatalf("finish jobs=%d", n)
	}
	f.g.mu.Lock()
	if len(f.g.finished) != 1 || f.g.finished[0] != "consume:tok-1" || f.g.tokens != 1 {
		t.Fatalf("finished=%v tokens=%d", f.g.finished, f.g.tokens)
	}
	f.g.mu.Unlock()
	// A verify after consumption is still a replay and grants nothing.
	f.Expect(f.verify("alpha", p, "gems_100", "tok-1"), 200)
	if f.gems("alpha", p) != 100 || f.runFinishJobs() != 0 {
		t.Fatal("re-delivered")
	}
	if mm, err := game.Reconcile(context.Background(), f.App.DB.Read, "alpha"); err != nil || len(mm) != 0 {
		t.Fatalf("reconcile %v %v", mm, err)
	}
	// Cross-game: alpha's purchase is looked up under beta's package and not found.
	b := f.Guest("beta")
	wantCode(t, f.verify("beta", b, "gems_100", "tok-1"), 400, "invalid_purchase")
	// Non-consumable mode (beta) acknowledges instead of consuming.
	f.g.purchase("tok-b", "gems_100", f.order("beta", b), func(p map[string]any) { p["_pkg"] = "com.beta.game" })
	f.Expect(f.verify("beta", b, "gems_100", "tok-b"), 200)
	if f.runFinishJobs() != 1 || f.gems("beta", b) != 100 {
		t.Fatal("beta delivery")
	}
	f.g.mu.Lock()
	defer f.g.mu.Unlock()
	if f.g.finished[1] != "acknowledge:tok-b" {
		t.Fatalf("finished=%v", f.g.finished)
	}
}

func TestGoogleRejectsBadPurchases(t *testing.T) {
	f := setupGoogle(t)
	a, b := f.Guest("alpha"), f.Guest("alpha")
	tokA := f.order("alpha", a)
	f.g.purchase("x-1", "gems_100", tokA)
	// Cross-player: B submits A's purchase (bound to A's token) before and after A.
	wantCode(t, f.verify("alpha", b, "gems_100", "x-1"), 403, "transaction_owned_by_other_player")
	f.Expect(f.verify("alpha", a, "gems_100", "x-1"), 200)
	wantCode(t, f.verify("alpha", b, "gems_100", "x-1"), 403, "transaction_owned_by_other_player")
	// No obfuscated account id / an id the server never issued.
	f.g.purchase("x-2", "gems_100", "")
	wantCode(t, f.verify("alpha", a, "gems_100", "x-2"), 400, "invalid_request")
	f.g.purchase("x-3", "gems_100", "6f1d4a8e-0000-4000-8000-000000000000")
	wantCode(t, f.verify("alpha", a, "gems_100", "x-3"), 403, "unknown_app_account_token")
	// Pending, canceled, test purchases; unknown token; wrong product.
	f.g.purchase("x-4", "gems_100", f.order("alpha", a), func(p map[string]any) { p["purchaseState"] = 2 })
	wantCode(t, f.verify("alpha", a, "gems_100", "x-4"), 409, "purchase_pending")
	f.g.purchase("x-5", "gems_100", f.order("alpha", a), func(p map[string]any) { p["purchaseState"] = 1 })
	wantCode(t, f.verify("alpha", a, "gems_100", "x-5"), 409, "transaction_refunded")
	f.g.purchase("x-6", "gems_100", f.order("alpha", a), func(p map[string]any) { p["purchaseType"] = 0; delete(p, "orderId") })
	wantCode(t, f.verify("alpha", a, "gems_100", "x-6"), 400, "test_purchase_rejected")
	f.g.purchase("x-7", "gems_100", f.order("alpha", a), func(p map[string]any) { delete(p, "orderId") })
	wantCode(t, f.verify("alpha", a, "gems_100", "x-7"), 400, "invalid_purchase")
	wantCode(t, f.verify("alpha", a, "gems_100", "never-issued"), 400, "invalid_purchase")
	f.g.purchase("x-8", "gems_500", f.order("alpha", a)) // ordered gems100, bought gems_500
	wantCode(t, f.verify("alpha", a, "gems_500", "x-8"), 409, "product_mismatch")
	f.g.purchase("x-9", "unmapped", f.order("alpha", a))
	wantCode(t, f.verify("alpha", a, "unmapped", "x-9"), 422, "product_not_mapped")
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/google/verify", a.Token, map[string]string{"purchase_token": "x-1"}), 400, "invalid_request")
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/google/verify", a.Token, map[string]string{"purchase_token": "x-1", "product_id": "gems_100", "signed_transaction": "y"}), 400, "invalid_request")
	// Store outage is 503 and grants nothing.
	f.g.mu.Lock()
	f.g.failGet = true
	f.g.mu.Unlock()
	f.g.purchase("x-10", "gems_100", f.order("alpha", a))
	wantCode(t, f.verify("alpha", a, "gems_100", "x-10"), 503, "store_unavailable")
	if f.gems("alpha", a) != 100 || f.gems("alpha", b) != 0 {
		t.Fatalf("balances a=%d b=%d", f.gems("alpha", a), f.gems("alpha", b))
	}
}

func TestGoogleConcurrentDuplicateVerify(t *testing.T) {
	f := setupGoogle(t)
	p := f.Guest("alpha")
	f.g.purchase("c-1", "gems_100", f.order("alpha", p))
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh, ok := 0, 0
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.verify("alpha", p, "gems_100", "c-1")
			mu.Lock()
			defer mu.Unlock()
			if r.Status == 200 {
				ok++
				if r.Body["replayed"] == false {
					fresh++
				}
			}
		}()
	}
	wg.Wait()
	if ok != 12 || fresh != 1 || f.gems("alpha", p) != 100 || f.runFinishJobs() != 1 {
		t.Fatalf("ok=%d fresh=%d", ok, fresh)
	}
}

func TestGoogleRTDN(t *testing.T) {
	f := setupGoogle(t)
	p := f.Guest("alpha")
	tok := f.order("alpha", p)
	f.g.purchase("r-1", "gems_100", tok)
	// ONE_TIME_PRODUCT_PURCHASED is re-checked with the API and delivered to the token's player.
	purchased := map[string]any{"version": "1.0", "packageName": gPkg, "eventTimeMillis": "1700000000000",
		"oneTimeProductNotification": map[string]any{"version": "1.0", "notificationType": 1, "purchaseToken": "r-1", "sku": "gems_100"}}
	r := f.Expect(f.push(t, "alpha", "m-1", purchased), 200)
	if r.Body["status"] != "processed" || f.gems("alpha", p) != 100 {
		t.Fatalf("%s", r.Raw)
	}
	if r := f.Expect(f.verify("alpha", p, "gems_100", "r-1"), 200); r.Body["replayed"] != true {
		t.Fatalf("%s", r.Raw)
	}
	// Replayed push (same messageId) is a duplicate.
	if r := f.Expect(f.push(t, "alpha", "m-1", purchased), 200); r.Body["duplicate"] != true {
		t.Fatalf("%s", r.Raw)
	}
	// Voided purchase reverses the grant; its re-delivery and a second void are no-ops.
	for i := range 2 {
		r := f.Expect(f.push(t, "alpha", "m-2", voided("r-1", 1)), 200)
		if r.Body["duplicate"] != (i > 0) {
			t.Fatalf("%s", r.Raw)
		}
	}
	if r := f.Expect(f.push(t, "alpha", "m-3", voided("r-1", 1)), 200); r.Body["status"] != "ignored" {
		t.Fatalf("%s", r.Raw)
	}
	if f.gems("alpha", p) != 0 || f.count(`SELECT count(*) FROM game_ledger WHERE source_key LIKE 'refund:google:%'`) != 1 {
		t.Fatal("refund not applied once")
	}
	// Partial (quantity-based) refunds and subscription voids are verified and ignored.
	if r := f.Expect(f.push(t, "alpha", "m-4", voided("r-1", 2)), 200); r.Body["status"] != "ignored" || !strings.Contains(r.Body["reason"].(string), "partial") {
		t.Fatalf("%s", r.Raw)
	}
	test := map[string]any{"version": "1.0", "packageName": gPkg, "testNotification": map[string]any{"version": "1.0"}}
	if r := f.Expect(f.push(t, "alpha", "m-5", test), 200); r.Body["status"] != "ignored" {
		t.Fatalf("%s", r.Raw)
	}
	// Notification for another package.
	other := voided("r-1", 1)
	other["packageName"] = "com.evil"
	wantCode(t, f.push(t, "alpha", "m-6", other), 400, "package_mismatch")
	// beta's package differs from alpha's notification.
	wantCode(t, f.push(t, "beta", "m-7", voided("r-1", 1)), 400, "package_mismatch")
}

func TestGoogleRTDNAuthentication(t *testing.T) {
	f := setupGoogle(t)
	body := pushBody("a-1", voided("zzz", 1))
	cases := map[string]string{
		"none":         "",
		"garbage":      "not-a-jwt",
		"wrong aud":    f.g.pushToken(t, func(c map[string]any) { c["aud"] = "https://evil.example" }),
		"wrong iss":    f.g.pushToken(t, func(c map[string]any) { c["iss"] = "https://evil.example" }),
		"wrong email":  f.g.pushToken(t, func(c map[string]any) { c["email"] = "other@example.com" }),
		"not verified": f.g.pushToken(t, func(c map[string]any) { c["email_verified"] = false }),
		"expired":      f.g.pushToken(t, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases["foreign key"] = signRS256(t, other, "k1", map[string]any{"iss": "accounts.google.com", "aud": gAudience, "email": gPushSA,
		"email_verified": true, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
	cases["unknown kid"] = signRS256(t, f.g.oidcKey, "k2", map[string]any{"iss": "accounts.google.com", "aud": gAudience, "email": gPushSA,
		"email_verified": true, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
	for name, tok := range cases {
		wantCode(t, f.Do("POST", "/v1/games/alpha/callbacks/google", tok, body), 401, "invalid_signature")
		_ = name
	}
	if f.count(`SELECT count(*) FROM commerce_inbox`) != 0 || f.count(`SELECT count(*) FROM commerce_orders`) != 0 {
		t.Fatal("unauthenticated push was recorded")
	}
	// The valid token is accepted.
	f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/google", f.g.pushToken(t), body), 200)
}

// A void that arrives before the purchase is verified records the purchase as
// refunded; nothing is ever granted for it.
func TestGoogleRefundBeforePurchase(t *testing.T) {
	f := setupGoogle(t)
	p := f.Guest("alpha")
	f.g.purchase("o-1", "gems_100", f.order("alpha", p))
	f.Expect(f.push(t, "alpha", "v-1", voided("o-1", 1)), 200)
	wantCode(t, f.verify("alpha", p, "gems_100", "o-1"), 409, "transaction_refunded")
	purchased := map[string]any{"packageName": gPkg, "oneTimeProductNotification": map[string]any{"notificationType": 1, "purchaseToken": "o-1", "sku": "gems_100"}}
	if r := f.Expect(f.push(t, "alpha", "v-2", purchased), 200); r.Body["status"] != "ignored" {
		t.Fatalf("%s", r.Raw)
	}
	if f.gems("alpha", p) != 0 || f.count(`SELECT count(*) FROM game_ledger`) != 0 || f.runFinishJobs() != 0 {
		t.Fatal("refunded purchase was granted")
	}
}

func (f *gfixture) count(q string, args ...any) int {
	f.T.Helper()
	var n int
	if err := f.App.DB.Read.QueryRow(q, args...).Scan(&n); err != nil {
		f.T.Fatal(err)
	}
	return n
}
