package commerce

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/pillar/gostack/core/httpx"
)

// Transaction is a store transaction whose signature and app binding were verified.
type Transaction struct {
	TransactionID         string
	OriginalTransactionID string
	ProviderProductID     string
	AppAccountToken       string // lower-case UUID or ""
	Environment           string
	Quantity              int
	Revoked               bool // the store reports the transaction as refunded/revoked
}

// Notification kinds.
const (
	NotifyPurchase = "purchase"
	NotifyRefund   = "refund"
	NotifyIgnored  = "ignored" // verified but not acted upon (e.g. TEST, renewal info)
)

// Notification is a verified server-to-server store notification.
type Notification struct {
	EventID string // unique per provider (Apple notificationUUID)
	Type    string // provider type, e.g. REFUND
	Kind    string // NotifyPurchase | NotifyRefund | NotifyIgnored
	Txn     *Transaction
}

// Verifier verifies store proofs for one IAP provider. Implementations do all
// network IO / crypto here, before any database transaction is opened.
// Errors are httpx errors (400 for invalid proofs, 404 provider_not_configured).
type Verifier interface {
	VerifyTransaction(ctx context.Context, gameID string, proof string) (*Transaction, error)
	VerifyNotification(ctx context.Context, gameID string, body []byte) (*Notification, error)
}

// AppleApp binds a game to an App Store app.
type AppleApp struct {
	BundleID     string
	Environments []string // accepted environments, e.g. ["Production"] or ["Production","Sandbox"]
}

// AppleVerifier verifies App Store Server API / StoreKit 2 signed JWS
// (signedTransactionInfo) and App Store Server Notifications V2 (signedPayload)
// offline: ES256 over the x5c chain, which must chain to Roots (Apple Root CA -
// G3 supplied by configuration; never downloaded or embedded), with Apple's
// marker extensions on the leaf and intermediate.
type AppleVerifier struct {
	Roots *x509.CertPool
	Apps  map[string]AppleApp // by game_id
	Now   func() time.Time
}

// NewAppleVerifier parses PEM root certificates.
func NewAppleVerifier(rootPEM []byte, apps map[string]AppleApp) (*AppleVerifier, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		return nil, errors.New("commerce: apple root CA PEM contains no certificates")
	}
	for g, a := range apps {
		if a.BundleID == "" || len(a.Environments) == 0 {
			return nil, errors.New("commerce: apple app for game " + g + " needs bundle_id and environments")
		}
		for _, e := range a.Environments {
			if e != "Production" && e != "Sandbox" {
				return nil, errors.New("commerce: apple environment must be Production or Sandbox")
			}
		}
	}
	return &AppleVerifier{Roots: pool, Apps: apps}, nil
}

var (
	oidAppleLeaf         = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}
	oidAppleIntermediate = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}
)

func invalidProof(msg string) error { return httpx.NewError(400, "invalid_signature", msg) }

type appleTxnPayload struct {
	TransactionID         string `json:"transactionId"`
	OriginalTransactionID string `json:"originalTransactionId"`
	BundleID              string `json:"bundleId"`
	ProductID             string `json:"productId"`
	AppAccountToken       string `json:"appAccountToken"`
	Environment           string `json:"environment"`
	Quantity              int    `json:"quantity"`
	RevocationDate        int64  `json:"revocationDate"`
}

type appleNotificationPayload struct {
	NotificationType string `json:"notificationType"`
	Subtype          string `json:"subtype"`
	NotificationUUID string `json:"notificationUUID"`
	Data             *struct {
		BundleID              string `json:"bundleId"`
		Environment           string `json:"environment"`
		SignedTransactionInfo string `json:"signedTransactionInfo"`
	} `json:"data"`
}

func (v *AppleVerifier) app(gameID string) (AppleApp, error) {
	a, ok := v.Apps[gameID]
	if !ok {
		return a, httpx.NewError(404, "provider_not_configured", "apple is not configured for this game")
	}
	return a, nil
}

func (v *AppleVerifier) checkApp(a AppleApp, bundle, env string) error {
	if bundle != a.BundleID {
		return httpx.NewError(400, "bundle_mismatch", "bundle id does not match this game")
	}
	if !slices.Contains(a.Environments, env) {
		return httpx.NewError(400, "environment_mismatch", "environment not accepted for this game")
	}
	return nil
}

// VerifyTransaction verifies a StoreKit 2 / App Store Server API signedTransactionInfo.
func (v *AppleVerifier) VerifyTransaction(_ context.Context, gameID, proof string) (*Transaction, error) {
	a, err := v.app(gameID)
	if err != nil {
		return nil, err
	}
	var p appleTxnPayload
	if err := v.verifyJWS(proof, &p); err != nil {
		return nil, err
	}
	if err := v.checkApp(a, p.BundleID, p.Environment); err != nil {
		return nil, err
	}
	return txnFrom(p)
}

func txnFrom(p appleTxnPayload) (*Transaction, error) {
	if p.TransactionID == "" || len(p.TransactionID) > 64 || p.ProductID == "" || len(p.ProductID) > 128 {
		return nil, invalidProof("transaction is missing transactionId or productId")
	}
	q := p.Quantity
	if q == 0 {
		q = 1
	}
	if q < 0 || q > 1000 {
		return nil, invalidProof("transaction quantity out of range")
	}
	return &Transaction{
		TransactionID: p.TransactionID, OriginalTransactionID: p.OriginalTransactionID,
		ProviderProductID: p.ProductID, AppAccountToken: strings.ToLower(p.AppAccountToken),
		Environment: p.Environment, Quantity: q, Revoked: p.RevocationDate != 0,
	}, nil
}

// VerifyNotification verifies an App Store Server Notification V2 body {"signedPayload": "..."}.
// Both the outer payload and the embedded signedTransactionInfo are verified.
func (v *AppleVerifier) VerifyNotification(_ context.Context, gameID string, body []byte) (*Notification, error) {
	a, err := v.app(gameID)
	if err != nil {
		return nil, err
	}
	var in struct {
		SignedPayload string `json:"signedPayload"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.SignedPayload == "" {
		return nil, httpx.BadRequest("body must be {\"signedPayload\": \"...\"}")
	}
	var p appleNotificationPayload
	if err := v.verifyJWS(in.SignedPayload, &p); err != nil {
		return nil, err
	}
	if p.NotificationUUID == "" || len(p.NotificationUUID) > 64 || p.Data == nil {
		return nil, invalidProof("notification is missing notificationUUID or data")
	}
	if err := v.checkApp(a, p.Data.BundleID, p.Data.Environment); err != nil {
		return nil, err
	}
	n := &Notification{EventID: p.NotificationUUID, Type: p.NotificationType, Kind: NotifyIgnored}
	switch p.NotificationType {
	case "ONE_TIME_CHARGE":
		n.Kind = NotifyPurchase
	case "REFUND", "REVOKE":
		n.Kind = NotifyRefund
	}
	if n.Kind == NotifyIgnored {
		return n, nil
	}
	if p.Data.SignedTransactionInfo == "" {
		return nil, invalidProof("notification is missing signedTransactionInfo")
	}
	var tp appleTxnPayload
	if err := v.verifyJWS(p.Data.SignedTransactionInfo, &tp); err != nil {
		return nil, err
	}
	if err := v.checkApp(a, tp.BundleID, tp.Environment); err != nil {
		return nil, err
	}
	if n.Txn, err = txnFrom(tp); err != nil {
		return nil, err
	}
	return n, nil
}

// verifyJWS checks an ES256 compact JWS whose header carries the x5c chain and
// decodes its payload into out.
func (v *AppleVerifier) verifyJWS(jws string, out any) error {
	if len(jws) > 64<<10 {
		return invalidProof("signed data too large")
	}
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return invalidProof("malformed JWS")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return invalidProof("malformed JWS header")
	}
	var hdr struct {
		Alg string   `json:"alg"`
		X5c []string `json:"x5c"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil || hdr.Alg != "ES256" || len(hdr.X5c) < 2 || len(hdr.X5c) > 3 {
		return invalidProof("JWS header must be ES256 with an x5c chain")
	}
	certs := make([]*x509.Certificate, len(hdr.X5c))
	for i, s := range hdr.X5c {
		der, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return invalidProof("malformed x5c certificate")
		}
		if certs[i], err = x509.ParseCertificate(der); err != nil {
			return invalidProof("malformed x5c certificate")
		}
	}
	leaf, inter := certs[0], certs[1]
	if !hasExt(leaf, oidAppleLeaf) || !hasExt(inter, oidAppleIntermediate) {
		return invalidProof("certificate chain is not an App Store signing chain")
	}
	pool := x509.NewCertPool()
	pool.AddCert(inter)
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	// The chain must end at a configured root; a root shipped in x5c is not trusted by itself.
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: v.Roots, Intermediates: pool, CurrentTime: now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return invalidProof("certificate chain is not trusted")
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return invalidProof("leaf key is not ECDSA")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return invalidProof("malformed ES256 signature")
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, h[:], r, s) {
		return invalidProof("signature does not verify")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return invalidProof("malformed JWS payload")
	}
	if err := json.Unmarshal(pb, out); err != nil {
		return invalidProof("malformed JWS payload")
	}
	return nil
}

func hasExt(c *x509.Certificate, oid asn1.ObjectIdentifier) bool {
	for _, e := range c.Extensions {
		if e.Id.Equal(oid) {
			return true
		}
	}
	return false
}
