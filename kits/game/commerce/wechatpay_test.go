package commerce_test

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game/gametest"
)

// WeChat Pay tests use an httptest fake of the JSAPI order endpoint and
// locally generated merchant/platform RSA keys and APIv3 key. No WeChat Pay
// endpoint is contacted.

const (
	wxMch    = "1900000001"
	wxSerial = "PUB_KEY_ID_0000000001"
	wxAppA   = "wxalpha0000000001"
	wxAppB   = "wxbeta00000000001"
)

type wxFake struct {
	srv        *httptest.Server
	merchant   *rsa.PrivateKey
	platform   *rsa.PrivateKey
	apiv3      []byte
	mu         sync.Mutex
	prepays    []map[string]any
	fail       int // next N calls answer 500
	tradeNoErr bool
	// orders answers GET /v3/pay/transactions/out-trade-no/{no} (absent: 404 ORDER_NOT_EXIST).
	orders      map[string]map[string]any
	queried     []string
	badQuerySig bool
}

func newWxFake(t *testing.T) *wxFake {
	w := &wxFake{apiv3: []byte("0123456789abcdef0123456789abcdef")}
	w.merchant, _ = rsa.GenerateKey(rand.Reader, 2048)
	w.platform, _ = rsa.GenerateKey(rand.Reader, 2048)
	authRe := regexp.MustCompile(`^WECHATPAY2-SHA256-RSA2048 mchid="([^"]+)",nonce_str="([^"]+)",signature="([^"]+)",timestamp="(\d+)",serial_no="([^"]+)"$`)
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := authRe.FindStringSubmatch(r.Header.Get("Authorization"))
		if otn, ok := strings.CutPrefix(r.URL.Path, "/v3/pay/transactions/out-trade-no/"); ok && r.Method == "GET" {
			sig := []byte{}
			if m != nil {
				sig, _ = base64.StdEncoding.DecodeString(m[3])
			}
			var sum [32]byte
			if m != nil {
				sum = sha256.Sum256([]byte("GET\n" + r.URL.RequestURI() + "\n" + m[4] + "\n" + m[2] + "\n\n"))
			}
			if m == nil || m[1] != wxMch || r.URL.Query().Get("mchid") != wxMch || rsa.VerifyPKCS1v15(&w.merchant.PublicKey, crypto.SHA256, sum[:], sig) != nil {
				rw.WriteHeader(401)
				rw.Write([]byte(`{"code":"SIGN_ERROR"}`))
				return
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			w.queried = append(w.queried, otn)
			o, ok := w.orders[otn]
			if !ok {
				rw.WriteHeader(404)
				rw.Write([]byte(`{"code":"ORDER_NOT_EXIST","message":"订单不存在"}`))
				return
			}
			resp, _ := json.Marshal(o)
			hdr := w.sign(resp, time.Now())
			if w.badQuerySig {
				hdr = w.sign([]byte("other"), time.Now())
			}
			for k, v := range hdr {
				rw.Header().Set(k, v)
			}
			rw.Write(resp)
			return
		}
		if r.URL.Path != "/v3/pay/transactions/jsapi" || r.Method != "POST" || m == nil || m[1] != wxMch || m[5] != "MERCHANTSERIAL" {
			rw.WriteHeader(401)
			rw.Write([]byte(`{"code":"SIGN_ERROR"}`))
			return
		}
		sig, _ := base64.StdEncoding.DecodeString(m[3])
		sum := sha256.Sum256([]byte("POST\n/v3/pay/transactions/jsapi\n" + m[4] + "\n" + m[2] + "\n" + string(body) + "\n"))
		if rsa.VerifyPKCS1v15(&w.merchant.PublicKey, crypto.SHA256, sum[:], sig) != nil {
			rw.WriteHeader(401)
			rw.Write([]byte(`{"code":"SIGN_ERROR"}`))
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.fail > 0 {
			w.fail--
			rw.WriteHeader(500)
			rw.Write([]byte(`{"code":"SYSTEM_ERROR"}`))
			return
		}
		if w.tradeNoErr {
			rw.WriteHeader(403)
			rw.Write([]byte(`{"code":"OUT_TRADE_NO_USED","message":"商户订单号重复"}`))
			return
		}
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		w.prepays = append(w.prepays, req)
		resp := []byte(`{"prepay_id":"wx2611` + strconv.Itoa(len(w.prepays)) + `"}`)
		for k, v := range w.sign(resp, time.Now()) {
			rw.Header().Set(k, v)
		}
		rw.Write(resp)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

// sign returns Wechatpay-* headers for body signed with the platform key.
func (w *wxFake) sign(body []byte, at time.Time) map[string]string {
	ts, nonce := strconv.FormatInt(at.Unix(), 10), "N"+strconv.FormatInt(at.UnixNano(), 36)
	sum := sha256.Sum256([]byte(ts + "\n" + nonce + "\n" + string(body) + "\n"))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, w.platform, crypto.SHA256, sum[:])
	return map[string]string{"Wechatpay-Serial": wxSerial, "Wechatpay-Timestamp": ts, "Wechatpay-Nonce": nonce,
		"Wechatpay-Signature": base64.StdEncoding.EncodeToString(sig)}
}

// notification builds an encrypted notification body.
func (w *wxFake) notification(id, event, originalType string, resource any, key []byte) []byte {
	pt, _ := json.Marshal(resource)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := "abcdefghijkl"
	ct := gcm.Seal(nil, []byte(nonce), pt, []byte(originalType))
	b, _ := json.Marshal(map[string]any{"id": id, "create_time": "2026-11-25T10:00:00+08:00", "event_type": event, "resource_type": "encrypt-resource",
		"summary": "x", "resource": map[string]any{"algorithm": "AEAD_AES_256_GCM", "ciphertext": base64.StdEncoding.EncodeToString(ct),
			"associated_data": originalType, "original_type": originalType, "nonce": nonce}})
	return b
}

func paid(outTradeNo, txnID string, total int64) map[string]any {
	return map[string]any{"appid": wxAppA, "mchid": wxMch, "out_trade_no": outTradeNo, "transaction_id": txnID, "trade_type": "JSAPI",
		"trade_state": "SUCCESS", "trade_state_desc": "支付成功", "bank_type": "OTHERS", "attach": "gems_100", "success_time": "2026-11-25T10:00:00+08:00",
		"payer": map[string]any{"openid": "o-alpha"}, "amount": map[string]any{"total": total, "payer_total": total, "currency": "CNY", "payer_currency": "CNY"}}
}

func refunded(outTradeNo, txnID string, total, refund int64) map[string]any {
	return map[string]any{"mchid": wxMch, "out_trade_no": outTradeNo, "transaction_id": txnID, "out_refund_no": "r-" + txnID, "refund_id": "50" + txnID,
		"refund_status": "SUCCESS", "success_time": "2026-11-25T11:00:00+08:00", "user_received_account": "x",
		"amount": map[string]any{"total": total, "refund": refund, "payer_total": total, "payer_refund": refund}}
}

type wfixture struct {
	*gametest.Env
	w *wxFake
}

func setupWeChat(t *testing.T) *wfixture {
	w := newWxFake(t)
	dir := t.TempDir()
	der, _ := x509.MarshalPKIXPublicKey(&w.platform.PublicKey)
	pubFile := filepath.Join(dir, "platform.pem")
	if err := os.WriteFile(pubFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOSTACK_TEST_WX_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(w.merchant)})))
	t.Setenv("GOSTACK_TEST_WX_V3", string(w.apiv3))
	f := &wfixture{w: w}
	f.Env = gametest.New(t, gametest.Options{Mutate: func(c *config.Config) {
		c.Game.Modules = []string{"commerce"}
		c.Game.Commerce.WeChatPay = &config.WeChatPayIAPConfig{MchID: wxMch, MerchantSerialNo: "MERCHANTSERIAL",
			MerchantPrivateKeyRef: "env:GOSTACK_TEST_WX_KEY", APIv3KeyRef: "env:GOSTACK_TEST_WX_V3",
			PlatformKeys: map[string]string{wxSerial: pubFile}, NotifyBaseURL: "https://api.example.com", APIBaseURL: w.srv.URL,
			Apps: map[string]config.WeChatPayAppConf{
				"alpha": {AppID: wxAppA, Prices: map[string]int64{"gems_100": 600}, Description: "Alpha gems"},
				"beta":  {AppID: wxAppB, Prices: map[string]int64{"gems_100": 600}},
			}}
	}})
	for _, g := range []string{"alpha", "beta"} {
		f.Expect(f.Do("PUT", "/v1/admin/games/"+g+"/products", f.AdminToken, map[string]any{
			"gems100": map[string]any{"grants": map[string]int{"gems": 100}, "provider_product_ids": map[string]string{"wechatpay": "gems_100"}},
			"free":    map[string]any{"grants": map[string]int{"gems": 1}},
		}), 200)
	}
	return f
}

// wxPlayer is a guest with a linked WeChat identity (openid) as WeChat login would create.
func (f *wfixture) wxPlayer(gameID, openid string) gametest.Player {
	f.T.Helper()
	p := f.Guest(gameID)
	ctx := context.Background()
	err := f.App.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO player_identities(game_id,provider,subject,player_id,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			gameID, "wechat", openid, p.ID, time.Now().UnixMilli(), time.Now().UnixMilli())
		return err
	})
	if err != nil {
		f.T.Fatal(err)
	}
	return p
}

// prepaid creates an order and its WeChat prepay; returns order id and out_trade_no.
func (f *wfixture) prepaid(gameID string, p gametest.Player) (string, string) {
	f.T.Helper()
	o := f.Expect(f.Do("POST", "/v1/games/"+gameID+"/iap/orders", p.Token, map[string]string{"product_id": "gems100"}), 201)
	id := o.Body["order_id"].(string)
	r := f.Expect(f.Do("POST", "/v1/games/"+gameID+"/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": id}), 200)
	return id, r.Body["out_trade_no"].(string)
}

func (f *wfixture) notify(gameID string, body []byte, hdr map[string]string) gametest.Resp {
	h := []string{"Content-Type", "application/json"}
	for k, v := range hdr {
		h = append(h, k, v)
	}
	return f.Do("POST", "/v1/games/"+gameID+"/callbacks/wechatpay", "", body, h...)
}

func (f *wfixture) send(gameID string, body []byte) gametest.Resp {
	return f.notify(gameID, body, f.w.sign(body, time.Now()))
}

func (f *wfixture) gems(gameID string, p gametest.Player) int64 {
	f.T.Helper()
	r := f.Expect(f.Do("GET", "/v1/games/"+gameID+"/wallet", p.Token, nil), 200)
	n, _ := r.Body["balances"].(map[string]any)["gems"].(float64)
	return int64(n)
}

func (f *wfixture) count(q string, args ...any) int {
	f.T.Helper()
	var n int
	if err := f.App.DB.Read.QueryRow(q, args...).Scan(&n); err != nil {
		f.T.Fatal(err)
	}
	return n
}

func wantFail(t *testing.T, r gametest.Resp, status int, msg string) {
	t.Helper()
	if r.Status != status || r.Body["code"] != "FAIL" || r.Body["message"] != msg {
		t.Fatalf("got %d %s want %d FAIL %s", r.Status, r.Raw, status, msg)
	}
}

func TestWeChatPayPrepayAndDelivery(t *testing.T) {
	f := setupWeChat(t)
	p := f.wxPlayer("alpha", "o-alpha")
	o := f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "gems100"}), 201)
	id := o.Body["order_id"].(string)
	r := f.Expect(f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": id}), 200)
	pay := r.Body["payment"].(map[string]any)
	otn := r.Body["out_trade_no"].(string)
	if r.Body["amount"] != float64(600) || pay["package"] != "prepay_id=wx26111" || pay["signType"] != "RSA" || pay["appId"] != wxAppA || len(otn) != 32 {
		t.Fatalf("%s", r.Raw)
	}
	// paySign = RSA-SHA256(merchant key, appId\ntimeStamp\nnonceStr\npackage\n).
	sig, _ := base64.StdEncoding.DecodeString(pay["paySign"].(string))
	sum := sha256.Sum256([]byte(wxAppA + "\n" + pay["timeStamp"].(string) + "\n" + pay["nonceStr"].(string) + "\n" + pay["package"].(string) + "\n"))
	if rsa.VerifyPKCS1v15(&f.w.merchant.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		t.Fatal("paySign does not verify")
	}
	f.w.mu.Lock()
	req := f.w.prepays[0]
	f.w.mu.Unlock()
	if req["out_trade_no"] != otn || req["attach"] != "gems_100" || req["appid"] != wxAppA || req["mchid"] != wxMch ||
		req["notify_url"] != "https://api.example.com/v1/games/alpha/callbacks/wechatpay" || req["payer"].(map[string]any)["openid"] != "o-alpha" ||
		req["amount"].(map[string]any)["total"] != float64(600) || req["description"] != "Alpha gems" {
		t.Fatalf("prepay request %v", req)
	}
	// A retried prepay returns the stored prepay_id without calling WeChat Pay.
	r = f.Expect(f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": id}), 200)
	if r.Body["payment"].(map[string]any)["package"] != "prepay_id=wx26111" || len(f.w.prepays) != 1 {
		t.Fatalf("%s", r.Raw)
	}
	// Payment notification delivers once; WeChat's success answer is 204 without body.
	n := f.w.notification("evt-1", "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200000001", 600), f.w.apiv3)
	for range 3 {
		r := f.send("alpha", n)
		if r.Status != 204 || len(r.Raw) != 0 {
			t.Fatalf("%d %s", r.Status, r.Raw)
		}
	}
	// Re-delivered with a new notification id: still one grant (one order per transaction).
	f.Expect(f.send("alpha", f.w.notification("evt-2", "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200000001", 600), f.w.apiv3)), 204)
	if f.gems("alpha", p) != 100 || f.count(`SELECT count(*) FROM game_ledger WHERE source_key='iap:wechatpay:4200000001'`) != 1 {
		t.Fatal("not delivered exactly once")
	}
	g := f.Expect(f.Do("GET", "/v1/games/alpha/iap/orders/"+id, p.Token, nil), 200).Body["order"].(map[string]any)
	if g["state"] != "delivered" || g["amount"] != float64(600) || g["provider_txn_id"] != "4200000001" {
		t.Fatalf("%v", g)
	}
	// Another player cannot read or pay this order.
	q := f.wxPlayer("alpha", "o-other")
	wantCode(t, f.Do("GET", "/v1/games/alpha/iap/orders/"+id, q.Token, nil), 404, "not_found")
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", q.Token, map[string]string{"order_id": id}), 404, "not_found")
	// Delivered orders cannot be prepaid again; WeChat has no client verify.
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": id}), 409, "invalid_state")
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/wechatpay/verify", p.Token, map[string]string{"signed_transaction": "x"}), 400, "invalid_request")
	// Full refund reverses the grant; replays are no-ops.
	rf := f.w.notification("evt-3", "REFUND.SUCCESS", "refund", refunded(otn, "4200000001", 600, 600), f.w.apiv3)
	f.Expect(f.send("alpha", rf), 204)
	f.Expect(f.send("alpha", rf), 204)
	if f.gems("alpha", p) != 0 || f.count(`SELECT count(*) FROM game_ledger WHERE source_key LIKE 'refund:wechatpay:%'`) != 1 {
		t.Fatal("refund not applied once")
	}
}

func TestWeChatPayRejectsBadNotifications(t *testing.T) {
	f := setupWeChat(t)
	p := f.wxPlayer("alpha", "o-alpha")
	_, otn := f.prepaid("alpha", p)
	good := f.w.notification("bad-0", "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200000100", 600), f.w.apiv3)
	// Signature over a different body.
	h := f.w.sign(good, time.Now())
	tampered := []byte(strings.Replace(string(good), "bad-0", "bad-x", 1))
	wantFail(t, f.notify("alpha", tampered, h), 401, "invalid_signature")
	// Missing headers, unknown serial, stale and future timestamps, foreign signing key.
	wantFail(t, f.notify("alpha", good, nil), 401, "invalid_signature")
	h = f.w.sign(good, time.Now())
	h["Wechatpay-Serial"] = "PUB_KEY_ID_9999"
	wantFail(t, f.notify("alpha", good, h), 401, "invalid_signature")
	wantFail(t, f.notify("alpha", good, f.w.sign(good, time.Now().Add(-10*time.Minute))), 401, "invalid_signature")
	wantFail(t, f.notify("alpha", good, f.w.sign(good, time.Now().Add(10*time.Minute))), 401, "invalid_signature")
	other := &wxFake{}
	other.platform, _ = rsa.GenerateKey(rand.Reader, 2048)
	wantFail(t, f.notify("alpha", good, other.sign(good, time.Now())), 401, "invalid_signature")
	// Resource encrypted with another APIv3 key.
	wantFail(t, f.send("alpha", f.w.notification("bad-1", "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200000101", 600), []byte("ffffffffffffffffffffffffffffffff"))), 400, "invalid_signature")
	// Wrong merchant / wrong app (also: alpha's order sent to beta's callback).
	wm := paid(otn, "4200000102", 600)
	wm["mchid"] = "1900000999"
	wantFail(t, f.send("alpha", f.w.notification("bad-2", "TRANSACTION.SUCCESS", "transaction", wm, f.w.apiv3)), 400, "merchant_mismatch")
	wa := paid(otn, "4200000103", 600)
	wa["appid"] = "wxevil"
	wantFail(t, f.send("alpha", f.w.notification("bad-3", "TRANSACTION.SUCCESS", "transaction", wa, f.w.apiv3)), 400, "app_mismatch")
	wantFail(t, f.send("beta", good), 400, "app_mismatch")
	// Amount differs from the order: recorded failed, nothing granted, WeChat retries.
	wantFail(t, f.send("alpha", f.w.notification("bad-4", "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200000104", 1), f.w.apiv3)), 409, "amount_mismatch")
	if f.count(`SELECT count(*) FROM commerce_inbox WHERE event_id='bad-4' AND status='failed'`) != 1 {
		t.Fatal("amount mismatch not recorded as failed")
	}
	if f.gems("alpha", p) != 0 || f.count(`SELECT count(*) FROM game_ledger`) != 0 {
		t.Fatal("granted on a bad notification")
	}
	if f.count(`SELECT count(*) FROM commerce_inbox WHERE status<>'failed'`) != 0 {
		t.Fatal("unverified notification recorded")
	}
	// The genuine notification still works.
	f.Expect(f.send("alpha", good), 204)
	if f.gems("alpha", p) != 100 {
		t.Fatal("genuine notification not delivered")
	}
}

// Refund notification before the payment notification: the order is recorded
// refunded and the later payment is ignored. Nothing is granted.
func TestWeChatPayRefundBeforePayment(t *testing.T) {
	f := setupWeChat(t)
	p := f.wxPlayer("alpha", "o-alpha")
	id, otn := f.prepaid("alpha", p)
	f.Expect(f.send("alpha", f.w.notification("o-1", "REFUND.SUCCESS", "refund", refunded(otn, "4200000200", 600, 600), f.w.apiv3)), 204)
	f.Expect(f.send("alpha", f.w.notification("o-2", "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200000200", 600), f.w.apiv3)), 204)
	o := f.Expect(f.Do("GET", "/v1/games/alpha/iap/orders/"+id, p.Token, nil), 200).Body["order"].(map[string]any)
	if o["state"] != "refunded" || f.gems("alpha", p) != 0 || f.count(`SELECT count(*) FROM game_ledger`) != 0 {
		t.Fatalf("%v", o)
	}
	// Partial refunds and non-success refund events are verified and ignored.
	_, otn2 := f.prepaid("alpha", p)
	f.Expect(f.send("alpha", f.w.notification("o-3", "TRANSACTION.SUCCESS", "transaction", paid(otn2, "4200000201", 600), f.w.apiv3)), 204)
	f.Expect(f.send("alpha", f.w.notification("o-4", "REFUND.SUCCESS", "refund", refunded(otn2, "4200000201", 600, 100), f.w.apiv3)), 204)
	f.Expect(f.send("alpha", f.w.notification("o-5", "REFUND.ABNORMAL", "refund", refunded(otn2, "4200000201", 600, 600), f.w.apiv3)), 204)
	if f.gems("alpha", p) != 100 || f.count(`SELECT count(*) FROM commerce_inbox WHERE event_id IN ('o-4','o-5') AND status='ignored'`) != 2 {
		t.Fatal("partial/abnormal refund handling")
	}
}

func TestWeChatPayConcurrentDuplicateNotifications(t *testing.T) {
	f := setupWeChat(t)
	p := f.wxPlayer("alpha", "o-alpha")
	_, otn := f.prepaid("alpha", p)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Half replay one notification id, half carry distinct ids for the same transaction.
			id := "dup"
			if i%2 == 1 {
				id = "dup-" + strconv.Itoa(i)
			}
			r := f.send("alpha", f.w.notification(id, "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200000300", 600), f.w.apiv3))
			mu.Lock()
			if r.Status == 204 {
				ok++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if ok != 12 || f.gems("alpha", p) != 100 || f.count(`SELECT count(*) FROM game_ledger`) != 1 {
		t.Fatalf("ok=%d gems=%d", ok, f.gems("alpha", p))
	}
}

func TestWeChatPayPrepayFailures(t *testing.T) {
	f := setupWeChat(t)
	// No WeChat identity: no openid to pay with.
	g := f.Guest("alpha")
	o := f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", g.Token, map[string]string{"product_id": "gems100"}), 201)
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", g.Token, map[string]string{"order_id": o.Body["order_id"].(string)}), 409, "wechat_identity_required")
	p := f.wxPlayer("alpha", "o-alpha")
	// Product without a wechatpay mapping.
	o = f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "free"}), 201)
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": o.Body["order_id"].(string)}), 422, "product_not_mapped")
	// WeChat Pay unavailable: 503, the amount is fixed on the order, the retry succeeds with the same parameters.
	o = f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "gems100"}), 201)
	id := o.Body["order_id"].(string)
	f.w.mu.Lock()
	f.w.fail = 1
	f.w.mu.Unlock()
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": id}), 503, "store_unavailable")
	if f.count(`SELECT count(*) FROM commerce_orders WHERE order_id=? AND amount=600 AND provider='wechatpay' AND prepay_id=''`, id) != 1 {
		t.Fatal("order not prepared before the call")
	}
	f.Expect(f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": id}), 200)
	// WeChat already has the out_trade_no with other parameters.
	o = f.Expect(f.Do("POST", "/v1/games/alpha/iap/orders", p.Token, map[string]string{"product_id": "gems100"}), 201)
	f.w.mu.Lock()
	f.w.tradeNoErr = true
	f.w.mu.Unlock()
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": o.Body["order_id"].(string)}), 409, "prepay_conflict")
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/wechatpay/prepay", p.Token, map[string]string{"order_id": "ord_missing"}), 404, "not_found")
	// Not configured in this deployment for apple; unknown provider route.
	wantCode(t, f.Do("POST", "/v1/games/alpha/iap/apple/verify", p.Token, map[string]string{"signed_transaction": "x"}), 404, "provider_not_configured")
}
