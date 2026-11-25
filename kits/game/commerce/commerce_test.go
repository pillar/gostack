package commerce_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/commerce"
	"github.com/pillar/gostack/kits/game/gametest"
)

// --- test PKI (stands in for Apple Root CA - G3; no Apple endpoint is contacted) ---

type chain struct {
	rootPEM []byte
	certs   [][]byte // leaf, intermediate, root (DER)
	key     *ecdsa.PrivateKey
}

func newChain(t *testing.T, appleOIDs bool) *chain {
	t.Helper()
	mk := func(tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) ([]byte, *x509.Certificate) {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return der, c
	}
	key := func() *ecdsa.PrivateKey { k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader); return k }
	now := time.Now()
	rk, ik, lk := key(), key(), key()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, rootC := mk(root, root, &rk.PublicKey, rk)
	inter := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Test WWDR"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "Test StoreKit"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature}
	if appleOIDs {
		inter.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}, Value: []byte{5, 0}}}
		leaf.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}, Value: []byte{5, 0}}}
	}
	interDER, interC := mk(inter, rootC, &ik.PublicKey, rk)
	leafDER, _ := mk(leaf, interC, &lk.PublicKey, ik)
	return &chain{rootPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), certs: [][]byte{leafDER, interDER, rootDER}, key: lk}
}

func (c *chain) sign(t *testing.T, payload any) string {
	t.Helper()
	x5c := []string{}
	for _, d := range c.certs {
		x5c = append(x5c, base64.StdEncoding.EncodeToString(d))
	}
	hb, _ := json.Marshal(map[string]any{"alg": "ES256", "x5c": x5c})
	pb, _ := json.Marshal(payload)
	in := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	h := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, h[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func txn(id, product, token string) map[string]any {
	return map[string]any{"transactionId": id, "originalTransactionId": id, "bundleId": "com.alpha.game", "productId": product,
		"appAccountToken": token, "environment": "Sandbox", "quantity": 1, "type": "Consumable", "signedDate": time.Now().UnixMilli()}
}

func (c *chain) notification(t *testing.T, uuid, typ string, tx map[string]any) map[string]any {
	return map[string]any{"signedPayload": c.sign(t, map[string]any{"notificationType": typ, "notificationUUID": uuid, "version": "2.0",
		"data": map[string]any{"bundleId": tx["bundleId"], "environment": tx["environment"], "signedTransactionInfo": c.sign(t, tx)}})}
}

// --- environment --------------------------------------------------------------

type fixture struct {
	*gametest.Env
	ch       *chain
	adKey    *ecdsa.PrivateKey
	mod      *commerce.Module
	hmacKey  []byte
	keyFetch int
	mu       sync.Mutex
}

func setup(t *testing.T) *fixture {
	f := &fixture{ch: newChain(t, true), hmacKey: []byte("shared-secret")}
	f.adKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&f.adKey.PublicKey)
	keySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.keyFetch++
		f.mu.Unlock()
		fmt.Fprintf(w, `{"keys":[{"keyId":3335741209,"pem":%q,"base64":""}]}`, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	}))
	t.Cleanup(keySrv.Close)
	apple, err := commerce.NewAppleVerifier(f.ch.rootPEM, map[string]commerce.AppleApp{
		"alpha": {BundleID: "com.alpha.game", Environments: []string{"Production", "Sandbox"}},
		"beta":  {BundleID: "com.beta.game", Environments: []string{"Production"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Env = gametest.New(t, gametest.Options{Routes: func(mux *http.ServeMux, a *app.App) {
		f.mod = &commerce.Module{Kit: a.Game, DB: a.DB, Outbox: a.Jobs,
			IAP: map[string]commerce.Verifier{"apple": apple},
			Ads: map[string]commerce.AdVerifier{
				"admob":    &commerce.AdMobVerifier{Keys: &commerce.AdMobKeyFetcher{URL: keySrv.URL}},
				"unityads": &commerce.HMACAdVerifier{Secrets: map[string][]byte{"alpha": f.hmacKey}},
			}}
		f.mod.Routes(mux)
	}})
	for _, g := range []string{"alpha", "beta"} {
		f.Expect(f.Do("PUT", "/v1/admin/games/"+g+"/products", f.AdminToken, map[string]any{
			"gems100": map[string]any{"grants": map[string]int{"gems": 100}, "provider_product_ids": map[string]string{"apple": "com." + g + ".gems100"}},
		}), 200)
		f.Expect(f.Do("PUT", "/v1/admin/games/"+g+"/ad-placements", f.AdminToken, map[string]any{
			"bonus": map[string]any{"grants": map[string]int{"coins": 10}, "daily_cap": 2},
		}), 200)
	}
	return f
}

func (f *fixture) order(p gametest.Player) string {
	f.T.Helper()
	r := f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "gems100"}), 201)
	return r.Body["app_account_token"].(string)
}

func (f *fixture) verify(p gametest.Player, signed string) gametest.Resp {
	return f.Do("POST", "/v1/games/alpha/iap/apple/verify", p.Token, map[string]string{"signed_transaction": signed})
}

func (f *fixture) balance(gameID string, p gametest.Player, asset string) int64 {
	f.T.Helper()
	r := f.Expect(f.Do("GET", "/v1/games/"+gameID+"/wallet", p.Token, nil), 200)
	n, _ := r.Body["balances"].(map[string]any)[asset].(float64)
	return int64(n)
}

func (f *fixture) count(q string, args ...any) int {
	f.T.Helper()
	var n int
	if err := f.App.DB.Read.QueryRow(q, args...).Scan(&n); err != nil {
		f.T.Fatal(err)
	}
	return n
}

func wantCode(t *testing.T, r gametest.Resp, status int, code string) {
	t.Helper()
	if r.Status != status || r.Code() != code {
		t.Fatalf("got %d %s want %d %s: %s", r.Status, r.Code(), status, code, r.Raw)
	}
}

// --- IAP ----------------------------------------------------------------------

func TestAppleVerifyDeliversOnce(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	signed := f.ch.sign(t, txn("1000001", "com.alpha.gems100", f.order(p)))
	r := f.Expect(f.verify(p, signed), 200)
	o := r.Body["order"].(map[string]any)
	if o["state"] != "delivered" || r.Body["replayed"] != false || o["product_id"] != "gems100" {
		t.Fatalf("%s", r.Raw)
	}
	r = f.Expect(f.verify(p, signed), 200)
	if r.Body["replayed"] != true {
		t.Fatalf("%s", r.Raw)
	}
	if b := f.balance("alpha", p, "gems"); b != 100 {
		t.Fatalf("gems=%d", b)
	}
	if n := f.count(`SELECT count(*) FROM jobs WHERE type='commerce.notify'`); n != 1 {
		t.Fatalf("outbox jobs=%d", n)
	}
	if mm, err := game.Reconcile(context.Background(), f.App.DB.Read, "alpha"); err != nil || len(mm) != 0 {
		t.Fatalf("reconcile %v %v", mm, err)
	}
}

func TestAppleConcurrentDuplicateVerify(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	signed := f.ch.sign(t, txn("2000001", "com.alpha.gems100", f.order(p)))
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh, ok := 0, 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.verify(p, signed)
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
	if ok != 16 || fresh != 1 {
		t.Fatalf("ok=%d fresh=%d", ok, fresh)
	}
	if n := f.count(`SELECT count(*) FROM game_ledger WHERE game_id='alpha' AND source_key='iap:apple:2000001'`); n != 1 {
		t.Fatalf("ledger rows=%d", n)
	}
	if b := f.balance("alpha", p, "gems"); b != 100 {
		t.Fatalf("gems=%d", b)
	}
}

func TestAppleCrossPlayerAndCrossGameReplay(t *testing.T) {
	f := setup(t)
	a, b := f.Guest("alpha"), f.Guest("alpha")
	signed := f.ch.sign(t, txn("3000001", "com.alpha.gems100", f.order(a)))
	// B submits A's transaction before A does: the token binds it to A.
	wantCode(t, f.verify(b, signed), 403, "transaction_owned_by_other_player")
	f.Expect(f.verify(a, signed), 200)
	// ... and after A received it.
	wantCode(t, f.verify(b, signed), 403, "transaction_owned_by_other_player")
	// Transaction carrying B's token submitted by A.
	wantCode(t, f.verify(a, f.ch.sign(t, txn("3000002", "com.alpha.gems100", f.order(b)))), 403, "transaction_owned_by_other_player")
	// No token / a token the server never issued.
	wantCode(t, f.verify(a, f.ch.sign(t, txn("3000003", "com.alpha.gems100", ""))), 400, "invalid_request")
	wantCode(t, f.verify(a, f.ch.sign(t, txn("3000004", "com.alpha.gems100", "6f1d4a8e-0000-4000-8000-000000000000"))), 403, "unknown_app_account_token")
	// Same transaction in another game: its bundle id does not match.
	c := f.Guest("beta")
	wantCode(t, f.Do("POST", "/v1/games/beta/iap/apple/verify", c.Token, map[string]string{"signed_transaction": signed}), 400, "bundle_mismatch")
	if f.balance("alpha", b, "gems") != 0 || f.balance("alpha", a, "gems") != 100 || f.balance("beta", c, "gems") != 0 {
		t.Fatal("unexpected balances")
	}
}

func TestAppleRejectsBadProofs(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	tok := f.order(p)
	good := f.ch.sign(t, txn("4000001", "com.alpha.gems100", tok))
	// Tampered payload keeps the old signature.
	parts := []byte(good)
	forged := txn("4000001", "com.alpha.gems100", tok)
	forged["quantity"] = 50
	fb, _ := json.Marshal(forged)
	i, j := indexDot(good, 0), indexDot(good, 1)
	tampered := string(parts[:i+1]) + base64.RawURLEncoding.EncodeToString(fb) + good[j:]
	wantCode(t, f.verify(p, tampered), 400, "invalid_signature")
	// Chain from a root that is not configured.
	wantCode(t, f.verify(p, newChain(t, true).sign(t, txn("4000002", "com.alpha.gems100", tok))), 400, "invalid_signature")
	// Trusted-looking chain without App Store marker extensions.
	other := newChain(t, false)
	wantCode(t, f.verify(p, other.sign(t, txn("4000003", "com.alpha.gems100", tok))), 400, "invalid_signature")
	wantCode(t, f.verify(p, "not.a.jws"), 400, "invalid_signature")
	// Wrong bundle / environment / unmapped product.
	wb := txn("4000004", "com.alpha.gems100", tok)
	wb["bundleId"] = "com.evil.game"
	wantCode(t, f.verify(p, f.ch.sign(t, wb)), 400, "bundle_mismatch")
	wantCode(t, f.verify(p, f.ch.sign(t, txn("4000005", "com.alpha.unknown", tok))), 422, "product_not_mapped")
	c := f.Guest("beta")
	sb := txn("4000006", "com.beta.gems100", "")
	sb["bundleId"] = "com.beta.game" // beta accepts Production only
	wantCode(t, f.Do("POST", "/v1/games/beta/iap/apple/verify", c.Token, map[string]string{"signed_transaction": f.ch.sign(t, sb)}), 400, "environment_mismatch")
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/google/verify", p.Token, map[string]string{"signed_transaction": "x"}), 404, "provider_not_configured")
	// Nothing was granted; the genuine transaction still works.
	if f.balance("alpha", p, "gems") != 0 {
		t.Fatal("granted on bad proof")
	}
	f.Expect(f.verify(p, good), 200)
}

func indexDot(s string, n int) int {
	for i := range len(s) {
		if s[i] == '.' {
			if n == 0 {
				return i
			}
			n--
		}
	}
	return -1
}

func TestAppleNotificationsDuplicateAndRefund(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	tx := txn("5000001", "com.alpha.gems100", f.order(p))
	// The purchase notification alone delivers to the token's player.
	r := f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", f.ch.notification(t, "n-1", "ONE_TIME_CHARGE", tx)), 200)
	if r.Body["status"] != "processed" || f.balance("alpha", p, "gems") != 100 {
		t.Fatalf("%s", r.Raw)
	}
	// Client verify afterwards is a replay.
	if r := f.Expect(f.verify(p, f.ch.sign(t, tx)), 200); r.Body["replayed"] != true {
		t.Fatalf("%s", r.Raw)
	}
	refund := f.ch.notification(t, "n-2", "REFUND", tx)
	for i := range 3 {
		r := f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", refund), 200)
		if r.Body["duplicate"] != (i > 0) {
			t.Fatalf("%d: %s", i, r.Raw)
		}
	}
	// A second refund event for the same transaction (different UUID) is ignored.
	r = f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", f.ch.notification(t, "n-3", "REVOKE", tx)), 200)
	if r.Body["status"] != "ignored" {
		t.Fatalf("%s", r.Raw)
	}
	if f.balance("alpha", p, "gems") != 0 {
		t.Fatal("refund not applied")
	}
	if n := f.count(`SELECT count(*) FROM game_ledger WHERE source_key LIKE 'refund:apple:5000001%'`); n != 1 {
		t.Fatalf("refund ledger rows=%d", n)
	}
	r = f.Expect(f.Do("GET", "/v1/admin/games/alpha/orders?state=refunded", f.AdminToken, nil), 200)
	if len(r.Body["orders"].([]any)) != 1 {
		t.Fatalf("%s", r.Raw)
	}
	// Bad signature on the callback route.
	bad := f.ch.notification(t, "n-4", "REFUND", tx)
	bad["signedPayload"] = bad["signedPayload"].(string)[:len(bad["signedPayload"].(string))-4] + "AAAA"
	wantCode(t, f.Do("POST", "/v1/games/alpha/callbacks/apple", "", bad), 400, "invalid_signature")
	// TEST notifications are verified and ignored.
	r = f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", map[string]any{"signedPayload": f.ch.sign(t, map[string]any{
		"notificationType": "TEST", "notificationUUID": "n-5", "data": map[string]any{"bundleId": "com.alpha.game", "environment": "Sandbox"}})}), 200)
	if r.Body["status"] != "ignored" {
		t.Fatalf("%s", r.Raw)
	}
}

// Out of order: the refund arrives before the purchase notification and before
// the client verifies. The order is recorded as refunded and is never delivered.
func TestAppleRefundBeforePurchase(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	tx := txn("6000001", "com.alpha.gems100", f.order(p))
	r := f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", f.ch.notification(t, "o-1", "REFUND", tx)), 200)
	if r.Body["status"] != "processed" {
		t.Fatalf("%s", r.Raw)
	}
	r = f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", f.ch.notification(t, "o-2", "ONE_TIME_CHARGE", tx)), 200)
	if r.Body["status"] != "ignored" {
		t.Fatalf("%s", r.Raw)
	}
	wantCode(t, f.verify(p, f.ch.sign(t, tx)), 409, "transaction_refunded")
	// Unknown transaction (no token known) refunded first, then verified by an unbound client.
	tx2 := txn("6000002", "com.alpha.gems100", "")
	f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", f.ch.notification(t, "o-3", "REFUND", tx2)), 200)
	f.mod.AllowUnboundTransactions = true
	wantCode(t, f.verify(p, f.ch.sign(t, tx2)), 409, "transaction_refunded")
	if f.balance("alpha", p, "gems") != 0 || f.count(`SELECT count(*) FROM game_ledger WHERE game_id='alpha'`) != 0 {
		t.Fatal("refunded transaction was granted")
	}
}

// Unbound mode: a paid notification without token waits; the first verifying player gets it once.
func TestAppleUnboundTransactions(t *testing.T) {
	f := setup(t)
	f.mod.AllowUnboundTransactions = true
	a, b := f.Guest("alpha"), f.Guest("alpha")
	tx := txn("7000001", "com.alpha.gems100", "")
	r := f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", f.ch.notification(t, "u-1", "ONE_TIME_CHARGE", tx)), 200)
	if r.Body["status"] != "processed" || f.count(`SELECT count(*) FROM commerce_orders WHERE state='paid'`) != 1 {
		t.Fatalf("%s", r.Raw)
	}
	f.Expect(f.verify(a, f.ch.sign(t, tx)), 200)
	wantCode(t, f.verify(b, f.ch.sign(t, tx)), 403, "transaction_owned_by_other_player")
	if f.balance("alpha", a, "gems") != 100 || f.balance("alpha", b, "gems") != 0 {
		t.Fatal("balances")
	}
}

// A notification that cannot be processed yet is recorded failed and retried on re-delivery.
func TestAppleFailedNotificationRetried(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	r := f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "gems100"}), 201)
	tx := txn("8000001", "com.alpha.gems500", r.Body["app_account_token"].(string))
	n := f.ch.notification(t, "f-1", "ONE_TIME_CHARGE", tx)
	wantCode(t, f.Do("POST", "/v1/games/alpha/callbacks/apple", "", n), 422, "product_not_mapped")
	if f.count(`SELECT count(*) FROM commerce_inbox WHERE event_id='f-1' AND status='failed'`) != 1 {
		t.Fatal("failed inbox record missing")
	}
	// Admin maps the store product to the ordered product; Apple re-delivers.
	f.Expect(f.Do("PUT", "/v1/admin/games/alpha/products", f.AdminToken, map[string]any{
		"gems100": map[string]any{"grants": map[string]int{"gems": 100}, "provider_product_ids": map[string]string{"apple": "com.alpha.gems500"}},
	}), 200)
	r = f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", n), 200)
	if r.Body["status"] != "processed" || f.balance("alpha", p, "gems") != 100 {
		t.Fatalf("%s", r.Raw)
	}
}

func TestRefundShortfall(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	tx := txn("9000001", "com.alpha.gems100", f.order(p))
	f.Expect(f.verify(p, f.ch.sign(t, tx)), 200)
	ctx := context.Background()
	adjust := func(key string, n int64) {
		err := f.App.DB.WriteTx(ctx, func(t *sql.Tx) error {
			return game.ApplyLedger(ctx, t, "alpha", p.ID, "spend", key, map[string]int64{"gems": n}, time.Now().UnixMilli())
		})
		if err != nil {
			f.T.Fatal(err)
		}
	}
	adjust("spend:1", -70)
	r := f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/apple", "", f.ch.notification(t, "s-1", "REFUND", tx)), 200)
	if r.Body["status"] != "processed" || f.balance("alpha", p, "gems") != 0 {
		t.Fatalf("%s", r.Raw)
	}
	r = f.Expect(f.Do("GET", "/v1/admin/games/alpha/orders?state=refund_shortfall", f.AdminToken, nil), 200)
	orders := r.Body["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("%s", r.Raw)
	}
	o := orders[0].(map[string]any)
	if o["outstanding"].(map[string]any)["gems"] != float64(70) || o["player_id"] != p.ID {
		t.Fatalf("%v", o)
	}
	id := o["order_id"].(string)
	adjust("gift:1", 50)
	r = f.Expect(f.Do("POST", "/v1/admin/games/alpha/orders/"+id+"/resolve", f.AdminToken, "{}"), 200)
	o = r.Body["order"].(map[string]any)
	if o["state"] != "refund_shortfall" || o["outstanding"].(map[string]any)["gems"] != float64(20) || f.balance("alpha", p, "gems") != 0 {
		t.Fatalf("%s", r.Raw)
	}
	r = f.Expect(f.Do("POST", "/v1/admin/games/alpha/orders/"+id+"/resolve", f.AdminToken, `{"waive":true}`), 200)
	if r.Body["order"].(map[string]any)["state"] != "refund_compensated" {
		t.Fatalf("%s", r.Raw)
	}
	wantCode(t, f.Do("POST", "/v1/admin/games/alpha/orders/"+id+"/resolve", f.AdminToken, "{}"), 409, "invalid_state")
	wantCode(t, f.Do("POST", "/v1/admin/games/beta/orders/"+id+"/resolve", f.AdminToken, "{}"), 404, "not_found")
	if mm, err := game.Reconcile(ctx, f.App.DB.Read, "alpha"); err != nil || len(mm) != 0 {
		t.Fatalf("reconcile %v %v", mm, err)
	}
}

// --- ads ----------------------------------------------------------------------

func (f *fixture) intent(gameID string, p gametest.Player) string {
	f.T.Helper()
	r := f.Expect(f.Do("POST", "/v1/games/"+gameID+"/ads/bonus/intent", p.Token, "{}"), 201)
	return r.Body["token"].(string)
}

func (f *fixture) admobQuery(t *testing.T, token, txID string) string {
	msg := "ad_network=5450213213286189855&ad_unit=1234567890&custom_data=" + url.QueryEscape(token) +
		"&reward_amount=1&reward_item=coins&timestamp=" + strconv.FormatInt(time.Now().UnixMilli(), 10) +
		"&transaction_id=" + txID + "&user_id=ignored"
	h := sha256.Sum256([]byte(msg))
	sig, err := ecdsa.SignASN1(rand.Reader, f.adKey, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return msg + "&signature=" + base64.RawURLEncoding.EncodeToString(sig) + "&key_id=3335741209"
}

func TestAdMobCallback(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	q := f.admobQuery(t, f.intent("alpha", p), "ev-1")
	for i := range 2 {
		r := f.Expect(f.Do("GET", "/v1/games/alpha/callbacks/ads/admob?"+q, "", nil), 200)
		if r.Body["status"] != "processed" || r.Body["duplicate"] != (i > 0) {
			t.Fatalf("%s", r.Raw)
		}
	}
	if f.balance("alpha", p, "coins") != 10 {
		t.Fatal("ad reward not granted once")
	}
	// The same intent token reported under a new transaction id is not credited again.
	r := f.Expect(f.Do("GET", "/v1/games/alpha/callbacks/ads/admob?"+f.admobQuery(t, f.intentTokenReuse(q), "ev-2"), "", nil), 200)
	if r.Body["status"] != "ignored" || r.Body["reason"] != "token_used" {
		t.Fatalf("%s", r.Raw)
	}
	// Forged player id / token never issued.
	r = f.Expect(f.Do("GET", "/v1/games/alpha/callbacks/ads/admob?"+f.admobQuery(t, p.ID, "ev-3"), "", nil), 200)
	if r.Body["reason"] != "unknown_token" {
		t.Fatalf("%s", r.Raw)
	}
	// A token issued in beta cannot be credited through alpha.
	bp := f.Guest("beta")
	r = f.Expect(f.Do("GET", "/v1/games/alpha/callbacks/ads/admob?"+f.admobQuery(t, f.intent("beta", bp), "ev-4"), "", nil), 200)
	if r.Body["reason"] != "unknown_token" {
		t.Fatalf("%s", r.Raw)
	}
	// Bad signature: altered signed field.
	tampered := f.admobQuery(t, f.intent("alpha", p), "ev-5")
	tampered = "ad_network=1" + tampered[len("ad_network=5450213213286189855"):]
	wantCode(t, f.Do("GET", "/v1/games/alpha/callbacks/ads/admob?"+tampered, "", nil), 401, "invalid_signature")
	wantCode(t, f.Do("GET", "/v1/games/alpha/callbacks/ads/admob?transaction_id=x", "", nil), 401, "invalid_signature")
	wantCode(t, f.Do("GET", "/v1/games/alpha/callbacks/ads/nope?"+q, "", nil), 404, "provider_not_configured")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keyFetch != 1 {
		t.Fatalf("key fetches=%d", f.keyFetch)
	}
}

func (f *fixture) intentTokenReuse(q string) string {
	v, _ := url.ParseQuery(q)
	return v.Get("custom_data")
}

func TestAdDailyCap(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	toks := []string{f.intent("alpha", p), f.intent("alpha", p), f.intent("alpha", p)}
	for i, tok := range toks {
		r := f.Expect(f.Do("GET", "/v1/games/alpha/callbacks/ads/admob?"+f.admobQuery(t, tok, "cap-"+strconv.Itoa(i)), "", nil), 200)
		want := "processed"
		if i == 2 {
			want = "ignored"
		}
		if r.Body["status"] != want {
			t.Fatalf("%d: %s", i, r.Raw)
		}
	}
	if f.balance("alpha", p, "coins") != 20 {
		t.Fatal("cap not enforced")
	}
	wantCode(t, f.Do("POST", "/v1/games/alpha/ads/bonus/intent", p.Token, "{}"), 409, "ad_cap_reached")
	wantCode(t, f.Do("POST", "/v1/games/alpha/ads/missing/intent", p.Token, "{}"), 404, "not_found")
	wantCode(t, f.Do("POST", "/v1/games/alpha/ads/bonus/intent", "", "{}"), 401, "unauthorized")
}

func hmacQuery(secret []byte, params map[string]string) string {
	v := url.Values{}
	for k, s := range params {
		v.Set(k, s)
	}
	keys := []string{}
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	canon := ""
	for i, k := range keys {
		if i > 0 {
			canon += "&"
		}
		canon += k + "=" + params[k]
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(canon))
	v.Set("sig", hex.EncodeToString(m.Sum(nil)))
	return v.Encode()
}

func TestHMACAdCallback(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	now := strconv.FormatInt(time.Now().Unix(), 10)
	q := hmacQuery(f.hmacKey, map[string]string{"event_id": "h-1", "token": f.intent("alpha", p), "ts": now})
	r := f.Expect(f.Do("GET", "/v1/games/alpha/callbacks/ads/unityads?"+q, "", nil), 200)
	if r.Body["status"] != "processed" {
		t.Fatalf("%s", r.Raw)
	}
	// Form-encoded POST duplicate.
	r = f.Expect(f.Do("POST", "/v1/games/alpha/callbacks/ads/unityads", "", q, "Content-Type", "application/x-www-form-urlencoded"), 200)
	if r.Body["duplicate"] != true || f.balance("alpha", p, "coins") != 10 {
		t.Fatalf("%s", r.Raw)
	}
	wantCode(t, f.Do("GET", "/v1/games/alpha/callbacks/ads/unityads?"+hmacQuery([]byte("wrong"), map[string]string{"event_id": "h-2", "token": f.intent("alpha", p), "ts": now}), "", nil), 401, "invalid_signature")
	old := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)
	wantCode(t, f.Do("GET", "/v1/games/alpha/callbacks/ads/unityads?"+hmacQuery(f.hmacKey, map[string]string{"event_id": "h-3", "token": f.intent("alpha", p), "ts": old}), "", nil), 401, "invalid_signature")
	// No secret configured for beta.
	wantCode(t, f.Do("GET", "/v1/games/beta/callbacks/ads/unityads?"+q, "", nil), 404, "provider_not_configured")
}

// --- admin & config -------------------------------------------------------------

func TestAdminValidationAndIsolation(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	wantCode(t, f.Do("PUT", "/v1/admin/games/alpha/products", f.AdminToken, map[string]any{
		"x": map[string]any{"grants": map[string]int{"gems": 1}, "provider_product_ids": map[string]string{"google": "sku"}}}), 400, "invalid_request")
	wantCode(t, f.Do("PUT", "/v1/admin/games/alpha/products", f.AdminToken, map[string]any{
		"x": map[string]any{"grants": map[string]int{"gems": 0}}}), 400, "invalid_request")
	wantCode(t, f.Do("PUT", "/v1/admin/games/alpha/products", f.AdminToken, map[string]any{
		"x": map[string]any{"grants": map[string]int{"gems": 1}, "provider_product_ids": map[string]string{"apple": "same"}},
		"y": map[string]any{"grants": map[string]int{"gems": 1}, "provider_product_ids": map[string]string{"apple": "same"}}}), 400, "invalid_request")
	wantCode(t, f.Do("PUT", "/v1/admin/games/alpha/ad-placements", f.AdminToken, map[string]any{
		"bonus": map[string]any{"grants": map[string]int{"coins": 1}, "daily_cap": 0}}), 400, "invalid_request")
	wantCode(t, f.Do("GET", "/v1/admin/games/alpha/orders?state=bogus", f.AdminToken, nil), 400, "invalid_request")
	f.Expect(f.Do("GET", "/v1/admin/games/alpha/products", p.Token, nil), 401)
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "nope"}), 404, "not_found")
	// Orders are idempotent with Idempotency-Key.
	r1 := f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "gems100"}, "Idempotency-Key", "k1"), 201)
	r2 := f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "gems100"}, "Idempotency-Key", "k1"), 201)
	if r1.Body["app_account_token"] != r2.Body["app_account_token"] || r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("%s %s", r1.Raw, r2.Raw)
	}
	// beta's admin view does not include alpha's orders.
	r := f.Expect(f.Do("GET", "/v1/admin/games/beta/orders", f.AdminToken, nil), 200)
	if len(r.Body["orders"].([]any)) != 0 {
		t.Fatalf("%s", r.Raw)
	}
	if err := f.mod.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProviders(t *testing.T) {
	if err := commerce.ValidateProviders([]string{"apple", "google", "wechatpay"}, []string{"admob", "hmac"}); err != nil {
		t.Fatal(err)
	}
	if commerce.ValidateProviders([]string{"huawei"}, nil) == nil || commerce.ValidateProviders(nil, []string{"wechat"}) == nil {
		t.Fatal("unimplemented provider accepted")
	}
	if _, err := commerce.NewAppleVerifier([]byte("nope"), nil); err == nil {
		t.Fatal("bad PEM accepted")
	}
}
