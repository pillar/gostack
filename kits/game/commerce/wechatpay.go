package commerce

import (
	"bytes"
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
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
)

// WeChat Pay API v3 (merchant, JSAPI / mini-program payment). This is NOT the
// WeChat mini-game virtual payment (Midas / 虚拟支付) API, which is not implemented.
const (
	WeChatPayBaseURL = "https://api.mch.weixin.qq.com"
	wechatPayJSAPI   = "/v3/pay/transactions/jsapi"
	wechatPayQuery   = "/v3/pay/transactions/out-trade-no/"
	// wechatPayOrderTTL bounds how long an order can be paid (time_expire =
	// created_at + TTL); a JSAPI prepay_id is valid for 2 hours.
	wechatPayOrderTTL = 2 * time.Hour
)

// WeChatPayApp binds a game to the AppID used for payment (the mini-program /
// official account AppID whose openid the player logged in with).
type WeChatPayApp struct {
	AppID string
	// Prices maps the wechatpay provider product id (admin catalog
	// provider_product_ids.wechatpay) to its price in fen (CNY).
	Prices map[string]int64
	// Description is the order description shown to the payer (default: product id).
	Description string
}

// WeChatPayMerchant holds the merchant API key used to sign requests to WeChat
// Pay and the client payment parameters.
type WeChatPayMerchant struct {
	MchID    string
	SerialNo string // merchant API certificate serial number
	Key      *rsa.PrivateKey
}

// sign returns base64(RSA-SHA256(key, message)).
func (m *WeChatPayMerchant) sign(message string) (string, error) {
	sum := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(nil, m.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// Authorization builds the WECHATPAY2-SHA256-RSA2048 header for a request:
// the signed message is METHOD\nURL\ntimestamp\nnonce\nbody\n (URL = path and query).
func (m *WeChatPayMerchant) Authorization(method, pathAndQuery string, body []byte, ts int64, nonce string) (string, error) {
	sig, err := m.sign(method + "\n" + pathAndQuery + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonce + "\n" + string(body) + "\n")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",signature="%s",timestamp="%d",serial_no="%s"`,
		m.MchID, nonce, sig, ts, m.SerialNo), nil
}

// WeChatPayPrepay is a JSAPI order (POST /v3/pay/transactions/jsapi).
type WeChatPayPrepay struct {
	AppID       string `json:"appid"`
	MchID       string `json:"mchid"`
	Description string `json:"description"`
	OutTradeNo  string `json:"out_trade_no"`
	TimeExpire  string `json:"time_expire,omitempty"`
	Attach      string `json:"attach,omitempty"`
	NotifyURL   string `json:"notify_url"`
	Amount      struct {
		Total    int64  `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
	Payer struct {
		OpenID string `json:"openid"`
	} `json:"payer"`
}

// WeChatPayClient creates and queries JSAPI orders. Called outside database transactions.
type WeChatPayClient interface {
	Prepay(ctx context.Context, req *WeChatPayPrepay) (prepayID string, err error)
	// QueryByOutTradeNo queries an order by merchant order number
	// (GET /v3/pay/transactions/out-trade-no/{out_trade_no}?mchid=...).
	// An order WeChat Pay does not know is a *WeChatPayError with Status 404.
	QueryByOutTradeNo(ctx context.Context, outTradeNo string) (*WeChatPayTransaction, error)
}

// WeChatPayError is a non-2xx answer from WeChat Pay.
type WeChatPayError struct {
	Status int
	Code   string
}

func (e *WeChatPayError) Error() string {
	return "wechatpay: HTTP " + strconv.Itoa(e.Status) + " " + e.Code
}

// WeChatPayHTTPClient signs requests with the merchant key and verifies the
// response signature with the platform keys.
type WeChatPayHTTPClient struct {
	BaseURL  string // default WeChatPayBaseURL
	Merchant *WeChatPayMerchant
	Verify   *WeChatPaySignatures
	Client   *http.Client // default 10s timeout
}

// do sends a merchant-signed request (the signed URL is the path and query)
// and returns the body of a 200 answer after verifying its Wechatpay-Signature.
func (c *WeChatPayHTTPClient) do(ctx context.Context, method, pathAndQuery string, body []byte) ([]byte, error) {
	base := c.BaseURL
	if base == "" {
		base = WeChatPayBaseURL
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+pathAndQuery, rd)
	if err != nil {
		return nil, err
	}
	authz, err := c.Merchant.Authorization(method, pathAndQuery, body, time.Now().Unix(), randomNonce())
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", authz)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gostack")
	hc := c.Client
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rb, &e)
		return nil, &WeChatPayError{Status: resp.StatusCode, Code: e.Code}
	}
	// A successful answer must carry a valid WeChat Pay signature.
	if err := c.Verify.Verify(resp.Header, rb); err != nil {
		return nil, &WeChatPayError{Status: resp.StatusCode, Code: "RESPONSE_SIGNATURE_INVALID"}
	}
	return rb, nil
}

func (c *WeChatPayHTTPClient) Prepay(ctx context.Context, p *WeChatPayPrepay) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	rb, err := c.do(ctx, http.MethodPost, wechatPayJSAPI, body)
	if err != nil {
		return "", err
	}
	var out struct {
		PrepayID string `json:"prepay_id"`
	}
	if json.Unmarshal(rb, &out) != nil || out.PrepayID == "" {
		return "", &WeChatPayError{Status: http.StatusOK, Code: "NO_PREPAY_ID"}
	}
	return out.PrepayID, nil
}

func (c *WeChatPayHTTPClient) QueryByOutTradeNo(ctx context.Context, outTradeNo string) (*WeChatPayTransaction, error) {
	rb, err := c.do(ctx, http.MethodGet, wechatPayQuery+url.PathEscape(outTradeNo)+"?mchid="+url.QueryEscape(c.Merchant.MchID), nil)
	if err != nil {
		return nil, err
	}
	var t WeChatPayTransaction
	if err := json.Unmarshal(rb, &t); err != nil {
		return nil, &WeChatPayError{Status: http.StatusOK, Code: "MALFORMED_RESPONSE"}
	}
	return &t, nil
}

func randomNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// WeChatPaySignatures verifies Wechatpay-Signature headers: RSA-SHA256 over
// "timestamp\nnonce\nbody\n" with the WeChat Pay platform public key (or
// platform certificate key) named by Wechatpay-Serial, and the timestamp
// within MaxSkew of now.
type WeChatPaySignatures struct {
	Keys    map[string]*rsa.PublicKey // by Wechatpay-Serial
	MaxSkew time.Duration             // default 5m
	Now     func() time.Time
}

// ParseWeChatPayPublicKey accepts a PEM public key or certificate.
func ParseWeChatPayPublicKey(pemBytes []byte) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("no PEM block")
	}
	var pub any
	var err error
	if blk.Type == "CERTIFICATE" {
		var c *x509.Certificate
		if c, err = x509.ParseCertificate(blk.Bytes); err == nil {
			pub = c.PublicKey
		}
	} else {
		pub, err = x509.ParsePKIXPublicKey(blk.Bytes)
	}
	if err != nil {
		return nil, err
	}
	k, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA public key")
	}
	return k, nil
}

func badWeChatSignature(msg string) error {
	return httpx.NewError(http.StatusUnauthorized, "invalid_signature", msg)
}

func (s *WeChatPaySignatures) Verify(h http.Header, body []byte) error {
	serial, tsS, nonce, sigS := h.Get("Wechatpay-Serial"), h.Get("Wechatpay-Timestamp"), h.Get("Wechatpay-Nonce"), h.Get("Wechatpay-Signature")
	if serial == "" || tsS == "" || nonce == "" || sigS == "" {
		return badWeChatSignature("missing Wechatpay-* signature headers")
	}
	key, ok := s.Keys[serial]
	if !ok {
		return badWeChatSignature("unknown Wechatpay-Serial")
	}
	ts, err := strconv.ParseInt(tsS, 10, 64)
	if err != nil {
		return badWeChatSignature("malformed Wechatpay-Timestamp")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	skew := s.MaxSkew
	if skew == 0 {
		skew = 5 * time.Minute
	}
	if d := now().Sub(time.Unix(ts, 0)); d > skew || d < -skew {
		return badWeChatSignature("Wechatpay-Timestamp outside the allowed window")
	}
	sig, err := base64.StdEncoding.DecodeString(sigS)
	if err != nil {
		return badWeChatSignature("malformed Wechatpay-Signature")
	}
	sum := sha256.Sum256([]byte(tsS + "\n" + nonce + "\n" + string(body) + "\n"))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return badWeChatSignature("signature does not verify")
	}
	return nil
}

// WeChatPayVerifier handles WeChat Pay API v3 payment and refund notifications
// and JSAPI prepay for one merchant.
type WeChatPayVerifier struct {
	MchID      string
	APIv3Key   []byte // 32 bytes, AEAD_AES_256_GCM key for notification resources
	Signatures *WeChatPaySignatures
	Merchant   *WeChatPayMerchant
	Client     WeChatPayClient
	Apps       map[string]WeChatPayApp // by game_id
	// NotifyBaseURL is the public origin of this server; notify_url is
	// NotifyBaseURL + /v1/games/{game}/callbacks/wechatpay.
	NotifyBaseURL string
	// QueryAfter is how long an order with a prepay_id may stay unpaid before
	// Module.Maintain queries it by out_trade_no (default 10m).
	QueryAfter time.Duration
	Log        *slog.Logger
}

func (v *WeChatPayVerifier) app(gameID string) (WeChatPayApp, error) {
	a, ok := v.Apps[gameID]
	if !ok {
		return a, httpx.NewError(http.StatusNotFound, "provider_not_configured", "wechatpay is not configured for this game")
	}
	return a, nil
}

// VerifyTransaction: WeChat Pay has no client-side proof; payments are
// delivered from the signed payment notification.
func (v *WeChatPayVerifier) VerifyTransaction(_ context.Context, gameID, _ string) (*Transaction, error) {
	if _, err := v.app(gameID); err != nil {
		return nil, err
	}
	return nil, httpx.BadRequest("wechatpay payments are delivered by the payment notification; poll GET /iap/orders/{order}")
}

func (v *WeChatPayVerifier) VerifyNotification(context.Context, string, []byte) (*Notification, error) {
	return nil, badWeChatSignature("missing Wechatpay-* signature headers")
}

// outTradeNo encodes an order's app_account_token (UUID) as a 32-hex
// out_trade_no; tokenFromOutTradeNo reverses it ("" when not ours).
func outTradeNo(token string) string { return strings.ReplaceAll(token, "-", "") }

func tokenFromOutTradeNo(s string) string {
	if len(s) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(s); err != nil {
		return ""
	}
	s = strings.ToLower(s)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

type wechatNotification struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
	Resource  *struct {
		Algorithm      string `json:"algorithm"`
		Ciphertext     string `json:"ciphertext"`
		AssociatedData string `json:"associated_data"`
		Nonce          string `json:"nonce"`
	} `json:"resource"`
}

// WeChatPayTransaction is a WeChat Pay API v3 transaction (payment
// notification resource, order query answer); only the fields used here.
type WeChatPayTransaction struct {
	AppID         string `json:"appid"`
	MchID         string `json:"mchid"`
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	TradeState    string `json:"trade_state"`
	Attach        string `json:"attach"`
	Amount        struct {
		Total    int64  `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

type wechatRefund struct {
	MchID         string `json:"mchid"`
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	RefundStatus  string `json:"refund_status"`
	Amount        struct {
		Total  int64 `json:"total"`
		Refund int64 `json:"refund"`
	} `json:"amount"`
}

func (v *WeChatPayVerifier) decrypt(n *wechatNotification, out any) error {
	r := n.Resource
	if r == nil || r.Algorithm != "AEAD_AES_256_GCM" {
		return invalidProof("resource must be AEAD_AES_256_GCM")
	}
	ct, err := base64.StdEncoding.DecodeString(r.Ciphertext)
	if err != nil {
		return invalidProof("malformed resource ciphertext")
	}
	block, err := aes.NewCipher(v.APIv3Key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(r.Nonce))
	if err != nil || len(r.Nonce) == 0 {
		return invalidProof("malformed resource nonce")
	}
	pt, err := gcm.Open(nil, []byte(r.Nonce), ct, []byte(r.AssociatedData))
	if err != nil {
		return invalidProof("resource does not decrypt with the APIv3 key")
	}
	if err := json.Unmarshal(pt, out); err != nil {
		return invalidProof("malformed resource plaintext")
	}
	return nil
}

var (
	errMerchantMismatch = httpx.NewError(http.StatusBadRequest, "merchant_mismatch", "notification is for another merchant")
	errAppMismatch      = httpx.NewError(http.StatusBadRequest, "app_mismatch", "notification is for another app")
)

// purchase maps a verified transaction (decrypted notification resource or
// signed order-query answer) to a purchase; reason explains a transaction
// that is not a deliverable payment (txn nil, no error).
func (v *WeChatPayVerifier) purchase(a WeChatPayApp, t *WeChatPayTransaction) (txn *Transaction, reason string, err error) {
	if t.MchID != v.MchID {
		return nil, "", errMerchantMismatch
	}
	if t.AppID != a.AppID {
		return nil, "", errAppMismatch
	}
	tok := tokenFromOutTradeNo(t.OutTradeNo)
	switch {
	case t.TradeState != "SUCCESS":
		return nil, "trade_state " + t.TradeState + " is not handled", nil
	case tok == "":
		return nil, "out_trade_no was not issued by this server", nil
	case t.TransactionID == "" || len(t.TransactionID) > 64 || t.Amount.Total <= 0 || (t.Amount.Currency != "" && t.Amount.Currency != "CNY"):
		return nil, "", invalidProof("transaction is missing transaction_id or a CNY amount")
	}
	return &Transaction{TransactionID: t.TransactionID, OriginalTransactionID: t.OutTradeNo, ProviderProductID: t.Attach,
		AppAccountToken: tok, Quantity: 1, Amount: t.Amount.Total}, "", nil
}

// VerifyNotificationRequest verifies the Wechatpay-Signature, decrypts the
// resource and maps TRANSACTION.SUCCESS to a purchase and a full
// REFUND.SUCCESS to a refund. out_trade_no carries the order's token.
func (v *WeChatPayVerifier) VerifyNotificationRequest(_ context.Context, gameID string, h http.Header, body []byte) (*Notification, error) {
	a, err := v.app(gameID)
	if err != nil {
		return nil, err
	}
	if err := v.Signatures.Verify(h, body); err != nil {
		return nil, err
	}
	var wn wechatNotification
	if err := json.Unmarshal(body, &wn); err != nil || wn.ID == "" || len(wn.ID) > 64 {
		return nil, httpx.BadRequest("body is not a WeChat Pay notification")
	}
	n := &Notification{EventID: wn.ID, Type: wn.EventType, Kind: NotifyIgnored}
	switch wn.EventType {
	case "TRANSACTION.SUCCESS":
		var t WeChatPayTransaction
		if err := v.decrypt(&wn, &t); err != nil {
			return nil, err
		}
		if n.Txn, n.Reason, err = v.purchase(a, &t); err != nil {
			return nil, err
		}
		if n.Txn != nil {
			n.Kind = NotifyPurchase
		}
	case "REFUND.SUCCESS":
		var rf wechatRefund
		if err := v.decrypt(&wn, &rf); err != nil {
			return nil, err
		}
		if rf.MchID != v.MchID {
			return nil, errMerchantMismatch
		}
		tok := tokenFromOutTradeNo(rf.OutTradeNo)
		switch {
		case rf.RefundStatus != "SUCCESS":
			n.Reason = "refund_status " + rf.RefundStatus + " is not handled"
		case tok == "":
			n.Reason = "out_trade_no was not issued by this server"
		case rf.Amount.Refund < rf.Amount.Total:
			n.Reason = "partial refund requires manual handling"
		case rf.TransactionID == "" || len(rf.TransactionID) > 64:
			return nil, invalidProof("refund is missing transaction_id")
		default:
			n.Kind = NotifyRefund
			n.Txn = &Transaction{TransactionID: rf.TransactionID, OriginalTransactionID: rf.OutTradeNo, AppAccountToken: tok, Quantity: 1}
		}
	}
	return n, nil
}

// writeCallback answers in WeChat Pay's format: 204 without body on success;
// 4xx/5xx with {"code":"FAIL","message":...} so WeChat Pay retries.
func (v *WeChatPayVerifier) writeCallback(w http.ResponseWriter, _ map[string]any, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status, msg := http.StatusInternalServerError, "internal error"
	var he *httpx.Error
	if errors.As(err, &he) {
		status, msg = he.Status, he.Code
	} else if v.Log != nil {
		v.Log.Error("commerce: wechatpay notification failed", "err", err)
	}
	httpx.WriteJSON(w, status, map[string]string{"code": "FAIL", "message": msg})
}

// --- prepay -------------------------------------------------------------------

// wechatPrepay creates the WeChat Pay JSAPI order for one of the player's
// created orders and returns wx.requestPayment parameters.
//
// Sequence (no network IO inside a transaction): (1) a WriteTx fixes provider,
// store product and amount on the order; (2) the merchant-signed JSAPI call
// with out_trade_no = the order token without dashes and deterministic
// parameters (attach, amount, time_expire = created_at + 2h); (3) a WriteTx
// stores prepay_id. A retry after a failure in (2) or (3) repeats (2) with the
// same parameters; once prepay_id is stored, retries return it without calling
// WeChat Pay.
func (m *Module) wechatPrepay(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	v, ok := m.IAP["wechatpay"].(*WeChatPayVerifier)
	if !ok {
		return errNotConfigured
	}
	s := game.CurrentSession(ctx)
	a, err := v.app(s.GameID)
	if err != nil {
		return err
	}
	body, err := game.ReadBody(r, 1024)
	if err != nil {
		return err
	}
	var in struct {
		OrderID string `json:"order_id"`
	}
	if err := game.DecodeStrict(body, &in); err != nil {
		return err
	}
	if in.OrderID == "" || len(in.OrderID) > 64 {
		return httpx.BadRequest("order_id is required")
	}
	o, err := orderByID(ctx, m.DB.Read, s.GameID, in.OrderID)
	if err != nil {
		return err
	}
	if o == nil || o.PlayerID != s.PlayerID {
		return httpx.NotFound("order not found")
	}
	if o.State != "created" || (o.Provider != "" && o.Provider != "wechatpay") {
		return httpx.Conflict("invalid_state", "order is "+o.State+" and cannot be paid")
	}
	expire := time.UnixMilli(o.CreatedAt).Add(wechatPayOrderTTL)
	if m.now().After(expire.Add(-time.Minute)) {
		return httpx.Conflict("order_expired", "order expired; create a new order")
	}
	if o.PrepayID != "" {
		return v.writePayment(w, a, o, o.PrepayID)
	}
	var openid string
	err = m.DB.Read.QueryRowContext(ctx, `SELECT subject FROM player_identities WHERE game_id=? AND provider='wechat' AND player_id=? ORDER BY created_at LIMIT 1`,
		s.GameID, s.PlayerID).Scan(&openid)
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.Conflict("wechat_identity_required", "player has no WeChat login identity (openid)")
	}
	if err != nil {
		return err
	}
	ppid, amount := o.ProviderProductID, o.Amount
	if amount == 0 {
		err := m.DB.Read.QueryRowContext(ctx, `SELECT provider_product_id FROM commerce_provider_products WHERE game_id=? AND provider='wechatpay' AND product_id=?`,
			s.GameID, o.ProductID).Scan(&ppid)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotMapped
		}
		if err != nil {
			return err
		}
		if amount = a.Prices[ppid]; amount <= 0 {
			return httpx.NewError(http.StatusUnprocessableEntity, "product_not_mapped", "no wechatpay price is configured for "+ppid)
		}
		// (1) Fix the price on the order before WeChat Pay is called.
		err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET provider='wechatpay', provider_product_id=?, amount=?, updated_at=?
				WHERE order_id=? AND state='created' AND provider IN ('','wechatpay') AND amount=0`, ppid, amount, m.now().UnixMilli(), o.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return httpx.Conflict("invalid_state", "order changed concurrently; retry")
			}
			return nil
		})
		if err != nil {
			return game.MapWriteErr(err)
		}
		o.Provider, o.ProviderProductID, o.Amount = "wechatpay", ppid, amount
	}
	desc := a.Description
	if desc == "" {
		desc = o.ProductID
	}
	if len(desc) > 127 {
		desc = desc[:127]
	}
	req := &WeChatPayPrepay{AppID: a.AppID, MchID: v.MchID, Description: desc, OutTradeNo: outTradeNo(o.Token),
		TimeExpire: expire.In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339), Attach: ppid,
		NotifyURL: strings.TrimRight(v.NotifyBaseURL, "/") + "/v1/games/" + s.GameID + "/callbacks/wechatpay"}
	req.Amount.Total, req.Amount.Currency = amount, "CNY"
	req.Payer.OpenID = openid
	// (2) Network call outside any transaction.
	prepayID, err := v.Client.Prepay(ctx, req)
	if err != nil {
		var we *WeChatPayError
		if errors.As(err, &we) && we.Code == "OUT_TRADE_NO_USED" {
			return httpx.Conflict("prepay_conflict", "WeChat Pay already has this order with other parameters; create a new order")
		}
		if v.Log != nil {
			v.Log.Warn("commerce: wechatpay prepay failed", "game", s.GameID, "order", o.ID, "err", err)
		}
		return errStoreUnavailable
	}
	// (3) Remember prepay_id for retries.
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET prepay_id=?, updated_at=? WHERE order_id=? AND prepay_id=''`, prepayID, m.now().UnixMilli(), o.ID)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	return v.writePayment(w, a, o, prepayID)
}

// writePayment returns the wx.requestPayment parameters; paySign is
// RSA-SHA256 over "appId\ntimeStamp\nnonceStr\npackage\n" with the merchant key.
func (v *WeChatPayVerifier) writePayment(w http.ResponseWriter, a WeChatPayApp, o *order, prepayID string) error {
	ts, nonce, pkg := strconv.FormatInt(time.Now().Unix(), 10), randomNonce(), "prepay_id="+prepayID
	sig, err := v.Merchant.sign(a.AppID + "\n" + ts + "\n" + nonce + "\n" + pkg + "\n")
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"order_id": o.ID, "out_trade_no": outTradeNo(o.Token), "amount": o.Amount, "currency": "CNY",
		"payment": map[string]string{"appId": a.AppID, "timeStamp": ts, "nonceStr": nonce, "package": pkg, "signType": "RSA", "paySign": sig},
	})
	return nil
}

// --- order query reconciliation ----------------------------------------------

// wechatPayQueryMargin keeps querying an order this long after its prepay
// expiry (time_expire = created_at + wechatPayOrderTTL), for payments that
// completed just before expiry.
const wechatPayQueryMargin = time.Hour

// reconcileWeChatPay covers lost payment notifications: every order with a
// prepay_id that is still created after QueryAfter and younger than its prepay
// expiry + margin is queried by out_trade_no (merchant-signed GET, response
// signature verified, outside any transaction). trade_state SUCCESS is
// delivered through the notification path (processEvent + notifyPurchase,
// inbox event "query:<transaction_id>"): the order and ledger uniqueness on the
// transaction id make it idempotent with a notification that arrives later or
// concurrently. At most 100 orders per run, oldest first; maintenance runs
// every 10 minutes, so an unpaid order is queried about (2h+1h-QueryAfter)/10m times.
func (m *Module) reconcileWeChatPay(ctx context.Context) error {
	v, ok := m.IAP["wechatpay"].(*WeChatPayVerifier)
	if !ok || v.Client == nil {
		return nil
	}
	after := v.QueryAfter
	if after <= 0 {
		after = 10 * time.Minute
	}
	now := m.now()
	type cand struct{ id, gameID, tenant, token string }
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT o.order_id, o.game_id, g.tenant_id, coalesce(o.app_account_token,'')
		FROM commerce_orders o JOIN games g ON g.game_id=o.game_id
		WHERE o.provider='wechatpay' AND o.state='created' AND o.prepay_id<>'' AND o.created_at<=? AND o.created_at>=?
		ORDER BY o.created_at, o.order_id LIMIT 100`,
		now.Add(-after).UnixMilli(), now.Add(-wechatPayOrderTTL-wechatPayQueryMargin).UnixMilli())
	if err != nil {
		return err
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.gameID, &c.tenant, &c.token); err != nil {
			rows.Close()
			return err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, c := range cands {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		a, err := v.app(c.gameID)
		if err != nil || c.token == "" {
			continue
		}
		t, err := v.Client.QueryByOutTradeNo(ctx, outTradeNo(c.token))
		var we *WeChatPayError
		if errors.As(err, &we) && we.Status == http.StatusNotFound {
			continue // ORDER_NOT_EXIST: never paid at WeChat Pay
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("commerce: wechatpay query of order %s: %w", c.id, err))
			continue
		}
		if t.OutTradeNo != outTradeNo(c.token) {
			errs = append(errs, fmt.Errorf("commerce: wechatpay query of order %s answered another out_trade_no", c.id))
			continue
		}
		txn, _, err := v.purchase(a, t)
		if err != nil {
			errs = append(errs, fmt.Errorf("commerce: wechatpay query of order %s: %w", c.id, err))
			continue
		}
		if txn == nil {
			continue // NOTPAY, CLOSED, REFUND, ...
		}
		g := game.Game{ID: c.gameID, TenantID: c.tenant}
		res, err := m.processEvent(ctx, g.ID, "wechatpay", "query:"+txn.TransactionID, NotifyPurchase, hashOf([]byte(txn.TransactionID)),
			func(tx *sql.Tx) (inboxState, error) { return m.notifyPurchase(ctx, tx, g, "wechatpay", txn) })
		if err != nil {
			errs = append(errs, fmt.Errorf("commerce: wechatpay delivery of queried order %s: %w", c.id, err))
			continue
		}
		m.log().Warn("commerce: wechatpay payment found by order query (notification missing or late)", "game", c.gameID,
			"order", c.id, "status", res["status"])
		m.counter("commerce_wechatpay_query_delivered_total", "")
	}
	return errors.Join(errs...)
}
