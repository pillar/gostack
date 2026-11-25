package commerce

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/jobs"
)

// Google Play endpoints (overridable for tests).
const (
	GoogleTokenURL       = "https://oauth2.googleapis.com/token"
	GooglePlayAPIBaseURL = "https://androidpublisher.googleapis.com"
	GoogleJWKSURL        = "https://www.googleapis.com/oauth2/v3/certs"
	googlePublisherScope = "https://www.googleapis.com/auth/androidpublisher"

	// JobGoogleFinish acknowledges or consumes a delivered Google Play purchase.
	JobGoogleFinish = "commerce.google.finish"
)

// GoogleApp binds a game to a Google Play app.
type GoogleApp struct {
	PackageName string
	// AllowTest accepts license-tester purchases (purchaseType 0). Off in production.
	AllowTest bool
	// Acknowledge finishes purchases with purchases.products.acknowledge
	// (non-consumables); the default is purchases.products.consume.
	Acknowledge bool
}

// GoogleRTDN authenticates Real-time developer notifications pushed by a Cloud
// Pub/Sub push subscription with OIDC authentication.
type GoogleRTDN struct {
	Audience            string // the subscription's push auth audience
	ServiceAccountEmail string // the subscription's push auth service account
	Keys                RSAKeySource
}

// GoogleVerifier verifies Google Play one-time product purchases with the
// Google Play Developer API (purchases.products.get) and RTDN messages.
// All network IO happens here, before any database transaction.
type GoogleVerifier struct {
	Apps map[string]GoogleApp // by game_id
	API  *GooglePlayAPI
	RTDN *GoogleRTDN // nil: the callback route answers provider_not_configured
	Now  func() time.Time
}

type googleProof struct {
	ProductID     string `json:"product_id"`
	PurchaseToken string `json:"purchase_token"`
}

// googleProductPurchase is the purchases.products resource (androidpublisher v3).
type googleProductPurchase struct {
	PurchaseState               int    `json:"purchaseState"`        // 0 purchased, 1 canceled, 2 pending
	ConsumptionState            int    `json:"consumptionState"`     // 0 yet to be consumed, 1 consumed
	AcknowledgementState        int    `json:"acknowledgementState"` // 0 yet to be acknowledged, 1 acknowledged
	PurchaseType                *int   `json:"purchaseType"`         // absent: real purchase; 0 test, 1 promo, 2 rewarded
	OrderID                     string `json:"orderId"`
	ProductID                   string `json:"productId"`
	Quantity                    int    `json:"quantity"`
	ObfuscatedExternalAccountID string `json:"obfuscatedExternalAccountId"`
}

var (
	errStoreUnavailable = httpx.NewError(http.StatusServiceUnavailable, "store_unavailable", "store API unavailable; retry later")
	errPurchasePending  = httpx.Conflict("purchase_pending", "purchase is pending payment")
	errTestPurchase     = httpx.NewError(http.StatusBadRequest, "test_purchase_rejected", "test purchases are not accepted for this game")
)

func invalidPurchase(msg string) error {
	return httpx.NewError(http.StatusBadRequest, "invalid_purchase", msg)
}

// googleTxnID is the provider transaction id: a purchase token identifies one
// purchase; it is long and opaque, so its SHA-256 is stored.
func googleTxnID(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (v *GoogleVerifier) app(gameID string) (GoogleApp, error) {
	a, ok := v.Apps[gameID]
	if !ok {
		return a, httpx.NewError(http.StatusNotFound, "provider_not_configured", "google is not configured for this game")
	}
	return a, nil
}

func (v *GoogleVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// VerifyTransaction checks a client purchase {product_id, purchase_token} with purchases.products.get.
func (v *GoogleVerifier) VerifyTransaction(ctx context.Context, gameID, proof string) (*Transaction, error) {
	a, err := v.app(gameID)
	if err != nil {
		return nil, err
	}
	var p googleProof
	if err := json.Unmarshal([]byte(proof), &p); err != nil || p.ProductID == "" || p.PurchaseToken == "" {
		return nil, httpx.BadRequest("product_id and purchase_token are required")
	}
	if len(p.ProductID) > 128 || len(p.PurchaseToken) > 4096 {
		return nil, httpx.BadRequest("product_id or purchase_token too long")
	}
	pp, err := v.API.GetProduct(ctx, a.PackageName, p.ProductID, p.PurchaseToken)
	if err != nil {
		return nil, err
	}
	return v.txn(a, p.ProductID, p.PurchaseToken, pp)
}

func (v *GoogleVerifier) txn(a GoogleApp, productID, token string, pp *googleProductPurchase) (*Transaction, error) {
	if pp.ProductID != "" && pp.ProductID != productID {
		return nil, invalidPurchase("purchase is for another product")
	}
	env := "Production"
	if pp.PurchaseType != nil && *pp.PurchaseType == 0 {
		if !a.AllowTest {
			return nil, errTestPurchase
		}
		env = "Test"
	}
	switch pp.PurchaseState {
	case 0, 1:
	case 2:
		return nil, errPurchasePending // orderId is not set while pending
	default:
		return nil, invalidPurchase("unknown purchaseState")
	}
	// The purchase token (verified by purchases.products.get) is the
	// transaction identity; orderId is informational. A standard billing-flow
	// purchase (purchaseType absent) is expected to carry one. purchaseType is
	// "only set if this purchase was not made using the standard in-app billing
	// flow" (test 0, promo 1, rewarded 2); such purchases are accepted without
	// orderId.
	if pp.PurchaseType == nil && pp.OrderID == "" {
		return nil, invalidPurchase("purchase has no orderId")
	}
	q := pp.Quantity
	if q == 0 {
		q = 1
	}
	if q < 0 || q > 1000 {
		return nil, invalidPurchase("purchase quantity out of range")
	}
	finished := pp.ConsumptionState == 1 || (a.Acknowledge && pp.AcknowledgementState == 1)
	return &Transaction{
		TransactionID: googleTxnID(token), OriginalTransactionID: pp.OrderID, ProviderProductID: productID,
		AppAccountToken: strings.ToLower(pp.ObfuscatedExternalAccountID), Environment: env, Quantity: q,
		Revoked: pp.PurchaseState == 1, PurchaseToken: token, Finished: finished,
	}, nil
}

// VerifyNotification is not used: RTDN is authenticated by the request's bearer token.
func (v *GoogleVerifier) VerifyNotification(ctx context.Context, gameID string, body []byte) (*Notification, error) {
	return v.VerifyNotificationRequest(ctx, gameID, http.Header{}, body)
}

type pubsubPush struct {
	Message *struct {
		Data      string `json:"data"`
		MessageID string `json:"messageId"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

type developerNotification struct {
	PackageName                string `json:"packageName"`
	OneTimeProductNotification *struct {
		NotificationType int    `json:"notificationType"`
		PurchaseToken    string `json:"purchaseToken"`
		SKU              string `json:"sku"`
	} `json:"oneTimeProductNotification"`
	VoidedPurchaseNotification *struct {
		PurchaseToken string `json:"purchaseToken"`
		OrderID       string `json:"orderId"`
		ProductType   int    `json:"productType"`
		RefundType    int    `json:"refundType"`
	} `json:"voidedPurchaseNotification"`
	TestNotification                *json.RawMessage `json:"testNotification"`
	SubscriptionNotification        *json.RawMessage `json:"subscriptionNotification"`
	PendingRefundReviewNotification *json.RawMessage `json:"pendingRefundReviewNotification"`
}

// VerifyNotificationRequest verifies a Pub/Sub push (OIDC bearer token) carrying
// an RTDN DeveloperNotification. Voided one-time purchases become refunds;
// ONE_TIME_PRODUCT_PURCHASED is re-checked with purchases.products.get.
func (v *GoogleVerifier) VerifyNotificationRequest(ctx context.Context, gameID string, h http.Header, body []byte) (*Notification, error) {
	a, err := v.app(gameID)
	if err != nil {
		return nil, err
	}
	if v.RTDN == nil {
		return nil, httpx.NewError(http.StatusNotFound, "provider_not_configured", "google real-time developer notifications are not configured")
	}
	if err := v.verifyPushToken(ctx, h.Get("Authorization")); err != nil {
		return nil, err
	}
	var push pubsubPush
	if err := json.Unmarshal(body, &push); err != nil || push.Message == nil || push.Message.MessageID == "" || len(push.Message.MessageID) > 64 {
		return nil, httpx.BadRequest("body must be a Pub/Sub push message with message.messageId")
	}
	data, err := base64.StdEncoding.DecodeString(push.Message.Data)
	if err != nil {
		return nil, httpx.BadRequest("message.data is not base64")
	}
	var dn developerNotification
	if err := json.Unmarshal(data, &dn); err != nil {
		return nil, httpx.BadRequest("message.data is not a DeveloperNotification")
	}
	if dn.PackageName != a.PackageName {
		return nil, httpx.NewError(http.StatusBadRequest, "package_mismatch", "notification is for another package")
	}
	n := &Notification{EventID: push.Message.MessageID, Kind: NotifyIgnored}
	switch {
	case dn.VoidedPurchaseNotification != nil:
		vp := dn.VoidedPurchaseNotification
		n.Type = "VOIDED_PURCHASE"
		switch {
		case vp.ProductType != 2:
			n.Reason = "voided subscription purchases are not handled"
		case vp.RefundType != 1:
			n.Reason = "quantity-based partial refund requires manual handling"
		case vp.PurchaseToken == "":
			return nil, httpx.BadRequest("voided purchase without purchaseToken")
		default:
			n.Kind = NotifyRefund
			n.Txn = &Transaction{TransactionID: googleTxnID(vp.PurchaseToken), OriginalTransactionID: vp.OrderID, Quantity: 1}
		}
	case dn.OneTimeProductNotification != nil:
		ot := dn.OneTimeProductNotification
		if ot.PurchaseToken == "" || ot.SKU == "" || len(ot.SKU) > 128 || len(ot.PurchaseToken) > 4096 {
			return nil, httpx.BadRequest("one-time product notification without purchaseToken or sku")
		}
		switch ot.NotificationType {
		case 1:
			n.Type = "ONE_TIME_PRODUCT_PURCHASED"
			pp, err := v.API.GetProduct(ctx, a.PackageName, ot.SKU, ot.PurchaseToken)
			if err != nil {
				return nil, err
			}
			txn, err := v.txn(a, ot.SKU, ot.PurchaseToken, pp)
			switch {
			case errors.Is(err, errPurchasePending):
				n.Reason = "purchase is pending"
			case errors.Is(err, errTestPurchase):
				n.Reason = "test purchase not accepted"
			case err != nil:
				return nil, err
			default:
				n.Kind, n.Txn = NotifyPurchase, txn
			}
		case 2:
			n.Type = "ONE_TIME_PRODUCT_CANCELED"
			n.Kind = NotifyRefund
			n.Txn = &Transaction{TransactionID: googleTxnID(ot.PurchaseToken), ProviderProductID: ot.SKU, Quantity: 1}
		default:
			n.Type = "ONE_TIME_PRODUCT_" + strconv.Itoa(ot.NotificationType)
		}
	case dn.TestNotification != nil:
		n.Type = "TEST"
	case dn.SubscriptionNotification != nil:
		n.Type = "SUBSCRIPTION"
	case dn.PendingRefundReviewNotification != nil:
		n.Type = "PENDING_REFUND_REVIEW"
	default:
		n.Type = "UNKNOWN"
	}
	return n, nil
}

func badPushToken(msg string) error {
	return httpx.NewError(http.StatusUnauthorized, "invalid_signature", msg)
}

// verifyPushToken checks the Pub/Sub push OIDC token: RS256 signed by a key in
// Google's JWKS, iss accounts.google.com, the configured aud, unexpired, and the
// configured push service account email with email_verified.
func (v *GoogleVerifier) verifyPushToken(ctx context.Context, authz string) error {
	tok, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || tok == "" || len(tok) > 8192 {
		return badPushToken("missing bearer token")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return badPushToken("malformed token")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return badPushToken("malformed token header")
	}
	var hdr struct{ Alg, Kid string }
	if err := json.Unmarshal(hb, &hdr); err != nil || hdr.Alg != "RS256" || hdr.Kid == "" {
		return badPushToken("token must be RS256 with a kid")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return badPushToken("malformed token signature")
	}
	pub, err := v.RTDN.Keys.RSAKey(ctx, hdr.Kid)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
		return badPushToken("token signature does not verify")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return badPushToken("malformed token claims")
	}
	var c struct {
		Iss           string          `json:"iss"`
		Aud           json.RawMessage `json:"aud"`
		Exp           int64           `json:"exp"`
		Iat           int64           `json:"iat"`
		Email         string          `json:"email"`
		EmailVerified bool            `json:"email_verified"`
	}
	if err := json.Unmarshal(pb, &c); err != nil {
		return badPushToken("malformed token claims")
	}
	now := v.now().Unix()
	const leeway = 60
	switch {
	case c.Iss != "accounts.google.com" && c.Iss != "https://accounts.google.com":
		return badPushToken("token issuer is not Google")
	case !audContains(c.Aud, v.RTDN.Audience):
		return badPushToken("token audience mismatch")
	case c.Exp == 0 || now > c.Exp+leeway || c.Iat > now+leeway:
		return badPushToken("token expired or not yet valid")
	case !c.EmailVerified || !strings.EqualFold(c.Email, v.RTDN.ServiceAccountEmail):
		return badPushToken("token is not from the configured push service account")
	}
	return nil
}

func audContains(raw json.RawMessage, want string) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == want
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		for _, a := range list {
			if a == want {
				return true
			}
		}
	}
	return false
}

// --- acknowledge / consume job ------------------------------------------------

type googleFinishJob struct {
	GameID    string `json:"game_id"`
	OrderID   string `json:"order_id"`
	Package   string `json:"package"`
	ProductID string `json:"product_id"`
	Token     string `json:"purchase_token"`
	Consume   bool   `json:"consume"`
}

// googleFinishMaxAttempts is the finish job's retry budget. Google refunds
// purchases that are not acknowledged within three days. The queue backs off
// exponentially with full jitter, capped at 1 h (modules/jobs Nack), so 200
// attempts span about 4 days on average (attempts 12+ wait 0-1 h, mean 30 min)
// and, with overwhelming probability, more than 3 days. Module.Maintain
// re-enqueues the job of an unfinished order if it still dies.
const googleFinishMaxAttempts = 200

// googleFinishWindow is how long after delivery reconciliation keeps
// re-enqueueing an unfinished purchase's job (Google's acknowledgement deadline).
const googleFinishWindow = 3 * 24 * time.Hour

func (v *GoogleVerifier) finishJobType() string { return JobGoogleFinish }

func (v *GoogleVerifier) finishJob(gameID, orderID string, txn *Transaction) (jobs.Job, bool) {
	a, ok := v.Apps[gameID]
	if !ok || txn.Finished || txn.PurchaseToken == "" {
		return jobs.Job{}, false
	}
	b, _ := json.Marshal(googleFinishJob{GameID: gameID, OrderID: orderID, Package: a.PackageName,
		ProductID: txn.ProviderProductID, Token: txn.PurchaseToken, Consume: !a.Acknowledge})
	return jobs.Job{Type: JobGoogleFinish, Payload: b, MaxAttempts: googleFinishMaxAttempts}, true
}

// finish acknowledges or consumes a delivered purchase. Network errors, 5xx,
// 401, 408 and 429 are transient. Any other 4xx (403: the service account
// lacks permission, 404, 400, ...) is first checked with purchases.products.get:
// a purchase that is already consumed/acknowledged counts as done; otherwise
// the rejection needs attention and the job keeps retrying, so the purchase is
// finished once the cause is fixed.
func (v *GoogleVerifier) finish(ctx context.Context, payload []byte) finishResult {
	var p googleFinishJob
	if err := json.Unmarshal(payload, &p); err != nil || p.Token == "" {
		return finishResult{Attention: "bad finish job payload", Err: errors.New("commerce: bad google finish job payload")}
	}
	r := finishResult{GameID: p.GameID, OrderID: p.OrderID}
	err := v.API.Finish(ctx, p.Package, p.ProductID, p.Token, p.Consume)
	var ae *googleAPIError
	switch {
	case err == nil:
		r.Done = true
	case errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != 401 && ae.Status != 408 && ae.Status != 429:
		if pp, gerr := v.API.GetProduct(ctx, p.Package, p.ProductID, p.Token); gerr == nil &&
			(pp.ConsumptionState == 1 || (!p.Consume && pp.AcknowledgementState == 1)) {
			r.Done = true
			break
		}
		r.Attention, r.Status, r.Err = err.Error(), ae.Status, err
	default:
		r.Err = err
	}
	return r
}

// --- Google Play Developer API client ----------------------------------------

// GooglePlayAPI calls androidpublisher v3 purchases.products with a service
// account access token.
type GooglePlayAPI struct {
	BaseURL string // default GooglePlayAPIBaseURL
	Tokens  *GoogleServiceAccount
	Client  *http.Client // default 10s timeout
}

type googleAPIError struct{ Status int }

func (e *googleAPIError) Error() string { return "google play api: HTTP " + strconv.Itoa(e.Status) }

func (a *GooglePlayAPI) url(pkg, productID, token, suffix string) string {
	base := a.BaseURL
	if base == "" {
		base = GooglePlayAPIBaseURL
	}
	return strings.TrimRight(base, "/") + "/androidpublisher/v3/applications/" + url.PathEscape(pkg) +
		"/purchases/products/" + url.PathEscape(productID) + "/tokens/" + url.PathEscape(token) + suffix
}

func (a *GooglePlayAPI) do(ctx context.Context, method, u string, out any) error {
	at, err := a.Tokens.AccessToken(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+at)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := a.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusUnauthorized {
		a.Tokens.invalidate()
	}
	if resp.StatusCode/100 != 2 {
		return &googleAPIError{Status: resp.StatusCode}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// GetProduct calls purchases.products.get. Unknown or invalid tokens are 400
// invalid_purchase; credential, quota and network failures are 503.
func (a *GooglePlayAPI) GetProduct(ctx context.Context, pkg, productID, token string) (*googleProductPurchase, error) {
	var p googleProductPurchase
	err := a.do(ctx, http.MethodGet, a.url(pkg, productID, token, ""), &p)
	var ae *googleAPIError
	switch {
	case errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 404 || ae.Status == 410):
		return nil, invalidPurchase("purchase token is not valid for this product")
	case err != nil:
		return nil, errStoreUnavailable
	}
	return &p, nil
}

// Finish calls purchases.products.consume or .acknowledge.
func (a *GooglePlayAPI) Finish(ctx context.Context, pkg, productID, token string, consume bool) error {
	suffix := ":acknowledge"
	if consume {
		suffix = ":consume"
	}
	return a.do(ctx, http.MethodPost, a.url(pkg, productID, token, suffix), nil)
}

// --- service account OAuth2 (JWT bearer grant, RFC 7523) -----------------------

// GoogleServiceAccount obtains OAuth2 access tokens for the androidpublisher
// scope by signing an RS256 JWT assertion with the service account key.
type GoogleServiceAccount struct {
	Email    string
	KeyID    string
	Key      *rsa.PrivateKey
	TokenURL string       // default GoogleTokenURL; also the assertion audience
	Client   *http.Client // default 10s timeout
	Now      func() time.Time

	mu     sync.Mutex
	token  string
	exp    time.Time
	flight *fetchFlight
}

// NewGoogleServiceAccount parses a service account JSON key. tokenURL overrides
// the key's token_uri (tests).
func NewGoogleServiceAccount(keyJSON []byte, tokenURL string) (*GoogleServiceAccount, error) {
	var k struct {
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(keyJSON, &k); err != nil {
		return nil, errors.New("commerce: google service account key is not JSON")
	}
	if k.ClientEmail == "" || k.PrivateKey == "" {
		return nil, errors.New("commerce: google service account key needs client_email and private_key")
	}
	key, err := ParseRSAPrivateKey([]byte(k.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("commerce: google service account private_key: %w", err)
	}
	if tokenURL == "" {
		tokenURL = k.TokenURI
	}
	if tokenURL == "" {
		tokenURL = GoogleTokenURL
	}
	return &GoogleServiceAccount{Email: k.ClientEmail, KeyID: k.PrivateKeyID, Key: key, TokenURL: tokenURL}, nil
}

// ParseRSAPrivateKey parses a PKCS#8 or PKCS#1 PEM RSA private key.
func ParseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		return rk, nil
	}
	return x509.ParsePKCS1PrivateKey(blk.Bytes)
}

func (s *GoogleServiceAccount) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *GoogleServiceAccount) invalidate() {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
}

// fetchFlight is one in-flight fetch shared by every caller that needs it.
type fetchFlight struct {
	done chan struct{} // closed when the fetch finished
	err  error         // set before done is closed
}

// flightTimeout bounds a shared fetch, which runs detached from the context
// of the caller that started it (that caller may give up; others still wait).
const flightTimeout = 30 * time.Second

// wait blocks until fl finishes or ctx ends.
func (fl *fetchFlight) wait(ctx context.Context) error {
	select {
	case <-fl.done:
		return fl.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AccessToken returns a cached token, refreshing it a minute before expiry.
// At most one refresh is in flight (no lock is held during network IO);
// concurrent callers wait for it as long as their own ctx allows.
func (s *GoogleServiceAccount) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.token != "" && s.now().Before(s.exp.Add(-time.Minute)) {
		tok := s.token
		s.mu.Unlock()
		return tok, nil
	}
	fl := s.flight
	if fl == nil {
		fl = &fetchFlight{done: make(chan struct{})}
		s.flight = fl
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightTimeout)
			defer cancel()
			tok, exp, err := s.fetchToken(fctx)
			s.mu.Lock()
			if err == nil {
				s.token, s.exp = tok, exp
			}
			fl.err = err
			s.flight = nil
			s.mu.Unlock()
			close(fl.done)
		}()
	}
	s.mu.Unlock()
	if err := fl.wait(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" { // invalidated meanwhile
		return "", errStoreUnavailable
	}
	return s.token, nil
}

// fetchToken performs the JWT bearer grant (network IO; no lock held).
func (s *GoogleServiceAccount) fetchToken(ctx context.Context) (string, time.Time, error) {
	now := s.now()
	hdr := map[string]string{"alg": "RS256", "typ": "JWT"}
	if s.KeyID != "" {
		hdr["kid"] = s.KeyID
	}
	hb, _ := json.Marshal(hdr)
	cb, _ := json.Marshal(map[string]any{"iss": s.Email, "scope": googlePublisherScope, "aud": s.TokenURL,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	in := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(nil, s.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", time.Time{}, err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion": {in + "." + base64.RawURLEncoding.EncodeToString(sig)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", time.Time{}, errStoreUnavailable
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out) != nil || out.AccessToken == "" {
		return "", time.Time{}, errStoreUnavailable
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600
	}
	return out.AccessToken, now.Add(time.Duration(out.ExpiresIn) * time.Second), nil
}

// --- Google OIDC signing keys ---------------------------------------------------

// RSAKeySource returns an RSA public key by key id.
type RSAKeySource interface {
	RSAKey(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// JWKSFetcher fetches and caches an RSA JSON Web Key Set (Google's OIDC
// signing keys). Fetches happen in the request before any database
// transaction; an unknown kid triggers a refetch at most once per MinRefresh,
// and cached keys are kept if a refetch fails.
type JWKSFetcher struct {
	URL        string        // default GoogleJWKSURL
	Client     *http.Client  // default 5s timeout
	TTL        time.Duration // default 1h
	MinRefresh time.Duration // default 1m

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
	flight      *fetchFlight
}

// RSAKey returns the key for kid. No lock is held during network IO and at
// most one fetch is in flight: a known but expired key is returned at once
// while a refresh runs in the background; a caller that needs the fetch (cold
// cache, unknown kid) waits for it as long as its own ctx allows.
func (f *JWKSFetcher) RSAKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	errUnavailable := httpx.NewError(http.StatusServiceUnavailable, "key_fetch_failed", "token verification keys unavailable")
	f.mu.Lock()
	ttl, minRefresh := f.TTL, f.MinRefresh
	if ttl == 0 {
		ttl = time.Hour
	}
	if minRefresh == 0 {
		minRefresh = time.Minute
	}
	k, ok := f.keys[kid]
	if ok && time.Since(f.fetchedAt) < ttl {
		f.mu.Unlock()
		return k, nil
	}
	if f.flight == nil && (f.lastAttempt.IsZero() || time.Since(f.lastAttempt) >= minRefresh) {
		f.lastAttempt = time.Now()
		fl := &fetchFlight{done: make(chan struct{})}
		f.flight = fl
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightTimeout)
			defer cancel()
			keys, err := f.fetch(fctx)
			f.mu.Lock()
			if err == nil {
				f.keys, f.fetchedAt = keys, time.Now()
			}
			fl.err = err
			f.flight = nil
			f.mu.Unlock()
			close(fl.done)
		}()
	}
	fl := f.flight
	f.mu.Unlock()
	if ok { // stale: serve the cached key; a failed refresh keeps it
		return k, nil
	}
	if fl != nil {
		if err := fl.wait(ctx); err != nil {
			return nil, errUnavailable
		}
	}
	f.mu.Lock()
	k, ok = f.keys[kid]
	f.mu.Unlock()
	if ok {
		return k, nil
	}
	return nil, badPushToken("unknown signing key")
}

func (f *JWKSFetcher) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	u := f.URL
	if u == "" {
		u = GoogleJWKSURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c := f.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct{ Kty, Kid, N, E string } `json:"keys"`
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&set); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: no RSA keys")
	}
	return out, nil
}
