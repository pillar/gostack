package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/jobs"
)

// Webhook request headers. The signature is
// "v1=" + hex(HMAC-SHA256(secret, timestamp + "." + body)); receivers should
// reject stale timestamps and deduplicate by the event id (deliveries are
// at-least-once).
const (
	HeaderEventID   = "GoStack-Webhook-Id"
	HeaderTimestamp = "GoStack-Webhook-Timestamp"
	HeaderSignature = "GoStack-Webhook-Signature"
)

// EventTypes are the events WebKit emits.
var EventTypes = []string{"order.created", "order.paid", "order.fulfilled", "order.cancelled", "order.refunded"}

// Webhook is an endpoint (the secret is only returned at creation).
type Webhook struct {
	ID        string   `json:"id"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Active    bool     `json:"active"`
	Secret    string   `json:"secret,omitempty"`
	CreatedAt int64    `json:"created_at"`
}

// Delivery is the state of one event for one endpoint.
type Delivery struct {
	EventID     string `json:"event_id"`
	WebhookID   string `json:"webhook_id"`
	Status      string `json:"status"`
	Attempts    int64  `json:"attempts"`
	LastStatus  int64  `json:"last_status"`
	LastError   string `json:"last_error,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	DeliveredAt *int64 `json:"delivered_at,omitempty"`
}

// Sign computes the signature header value for body at timestamp ts.
func Sign(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}

// Verify checks a signature header (for receivers and tests) and the
// timestamp tolerance.
func Verify(secret, header string, ts int64, body []byte, now time.Time, tolerance time.Duration) bool {
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return false
	}
	return hmac.Equal([]byte(header), []byte(Sign(secret, ts, body)))
}

// CreateWebhook registers an endpoint and returns it with its signing secret
// (shown once; it is stored because every delivery is signed with it).
func (k *Kit) CreateWebhook(ctx context.Context, tenant, rawURL string, events []string) (Webhook, error) {
	u, err := url.Parse(rawURL)
	if err != nil || len(rawURL) > maxURL || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return Webhook{}, httpx.BadRequest("url must be an absolute http(s) URL")
	}
	if k.Production && u.Scheme != "https" {
		return Webhook{}, httpx.BadRequest("production: webhook url must use https")
	}
	// Literal IPs are checked now; host names are checked on every resolved
	// address at dial time.
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !k.Cfg.WebhookAllowPrivateNetworks && !PublicIP(ip) {
		return Webhook{}, httpx.BadRequest("webhook url must not point to a private or special-purpose address")
	}
	for _, e := range events {
		if !slices.Contains(EventTypes, e) {
			return Webhook{}, httpx.BadRequest("unknown event type " + e)
		}
	}
	if events == nil {
		events = []string{}
	}
	now := k.now().UnixMilli()
	h := Webhook{ID: httpx.RandomID("wh_"), URL: rawURL, Events: events, Active: true, Secret: auth.NewSecret("whsec_"), CreatedAt: now}
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM web_webhooks WHERE tenant_id=? AND active=1`, tenant).Scan(&n); err != nil {
			return err
		}
		if n >= 20 {
			return httpx.Conflict("conflict", "at most 20 active webhooks per tenant")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO web_webhooks(tenant_id,id,url,secret,events,active,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`,
			tenant, h.ID, h.URL, h.Secret, strings.Join(events, " "), now, now)
		return err
	})
	return h, err
}

// ListWebhooks lists endpoints without secrets.
func (k *Kit) ListWebhooks(ctx context.Context, tenant string) ([]Webhook, error) {
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT id,url,events,active,created_at FROM web_webhooks WHERE tenant_id=? ORDER BY created_at, id`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		var h Webhook
		var events string
		var active int64
		if err := rows.Scan(&h.ID, &h.URL, &events, &active, &h.CreatedAt); err != nil {
			return nil, err
		}
		h.Events, h.Active = strings.Fields(events), active != 0
		if h.Events == nil {
			h.Events = []string{}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// DisableWebhook stops future deliveries (pending ones end as failed).
func (k *Kit) DisableWebhook(ctx context.Context, tenant, id string) error {
	return k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE web_webhooks SET active=0, updated_at=? WHERE tenant_id=? AND id=?`, k.now().UnixMilli(), tenant, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return httpx.NotFound("webhook not found")
		}
		return nil
	})
}

// Deliveries lists the latest deliveries of an endpoint.
func (k *Kit) Deliveries(ctx context.Context, tenant, id string) ([]Delivery, error) {
	var n int
	if err := k.DB.Read.QueryRowContext(ctx, `SELECT count(*) FROM web_webhooks WHERE tenant_id=? AND id=?`, tenant, id).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, httpx.NotFound("webhook not found")
	}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT event_id,webhook_id,status,attempts,last_status,last_error,created_at,updated_at,delivered_at
		FROM web_webhook_deliveries WHERE tenant_id=? AND webhook_id=? ORDER BY created_at DESC, event_id DESC LIMIT 200`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.EventID, &d.WebhookID, &d.Status, &d.Attempts, &d.LastStatus, &d.LastError, &d.CreatedAt, &d.UpdatedAt, &d.DeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type webhookJob struct {
	EventID   string `json:"event_id"`
	WebhookID string `json:"webhook_id"`
}

// emit appends an event and enqueues one delivery per subscribed endpoint,
// inside the caller's transaction (transactional outbox).
func (k *Kit) emit(ctx context.Context, tx *sql.Tx, tenant, typ, from string, o Order, now int64) error {
	id := httpx.RandomID("evt_")
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"id": id, "type": typ, "created_at": now, "data": json.RawMessage(data)})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO web_events(id,tenant_id,type,order_id,order_version,from_status,to_status,payload,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, tenant, typ, o.ID, o.Version, from, o.Status, payload, now); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, events FROM web_webhooks WHERE tenant_id=? AND active=1 ORDER BY id`, tenant)
	if err != nil {
		return err
	}
	var hooks []string
	for rows.Next() {
		var hid, events string
		if err := rows.Scan(&hid, &events); err != nil {
			rows.Close()
			return err
		}
		if f := strings.Fields(events); len(f) == 0 || slices.Contains(f, typ) {
			hooks = append(hooks, hid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, hid := range hooks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO web_webhook_deliveries(tenant_id,event_id,webhook_id,status,created_at,updated_at) VALUES(?,?,?,'pending',?,?)`,
			tenant, id, hid, now, now); err != nil {
			return err
		}
		b, _ := json.Marshal(webhookJob{EventID: id, WebhookID: hid})
		if _, err := k.Jobs.Enqueue(ctx, tx, jobs.Job{TenantID: tenant, Type: JobWebhook, Payload: b,
			MaxAttempts: k.Cfg.WebhookMaxAttempts, DedupeKey: "webhook:" + id + ":" + hid}); err != nil {
			return err
		}
	}
	return nil
}

// handleWebhook delivers one event to one endpoint. Non-2xx responses and
// network errors return an error so the queue retries with backoff; the last
// attempt marks the delivery failed. A delivered event is never re-sent by a
// repeated job; a crash between sending and recording may re-send it
// (at-least-once), with the same event id.
func (k *Kit) handleWebhook(ctx context.Context, c *jobs.Claimed) error {
	var j webhookJob
	if err := json.Unmarshal(c.Payload, &j); err != nil {
		return nil
	}
	var status, hookURL, secret string
	var active int64
	var payload []byte
	err := k.DB.Read.QueryRowContext(ctx, `SELECT d.status, h.url, h.secret, h.active, e.payload
		FROM web_webhook_deliveries d
		JOIN web_webhooks h ON h.tenant_id=d.tenant_id AND h.id=d.webhook_id
		JOIN web_events e ON e.id=d.event_id
		WHERE d.tenant_id=? AND d.event_id=? AND d.webhook_id=?`, c.TenantID, j.EventID, j.WebhookID).Scan(&status, &hookURL, &secret, &active, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "pending" {
		return nil
	}
	if active == 0 {
		return k.recordDelivery(ctx, c.TenantID, j, "failed", 0, "endpoint disabled")
	}
	code, derr := k.send(ctx, hookURL, secret, j.EventID, payload)
	if derr == nil {
		return k.recordDelivery(ctx, c.TenantID, j, "delivered", code, "")
	}
	final := "pending"
	if c.Attempt >= k.Cfg.WebhookMaxAttempts {
		final = "failed"
	}
	if err := k.recordDelivery(ctx, c.TenantID, j, final, code, derr.Error()); err != nil {
		return err
	}
	return derr
}

func (k *Kit) recordDelivery(ctx context.Context, tenant string, j webhookJob, status string, code int, msg string) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		now := k.now().UnixMilli()
		var delivered any
		if status == "delivered" {
			delivered = now
		}
		_, err := tx.ExecContext(ctx, `UPDATE web_webhook_deliveries SET status=?, attempts=attempts+1, last_status=?, last_error=?, updated_at=?, delivered_at=?
			WHERE tenant_id=? AND event_id=? AND webhook_id=? AND status='pending'`, status, code, msg, now, delivered, tenant, j.EventID, j.WebhookID)
		return err
	})
}

// send performs one signed POST (outside any transaction).
func (k *Kit) send(ctx context.Context, target, secret, eventID string, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, k.Cfg.WebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := k.now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GoStack-Webhook/1")
	req.Header.Set(HeaderEventID, eventID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, Sign(secret, ts, body))
	res, err := k.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("webhook delivery: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, fmt.Errorf("webhook endpoint answered %d", res.StatusCode)
	}
	return res.StatusCode, nil
}

// client returns k.Client or the Kit's default guarded client, built once and
// reused so that deliveries share one connection pool.
func (k *Kit) client() *http.Client {
	if k.Client != nil {
		return k.Client
	}
	k.clientOnce.Do(func() { k.defClient = NewWebhookClient(k.Cfg.WebhookAllowPrivateNetworks) })
	return k.defClient
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Special-purpose ranges (IANA IPv4/IPv6 special-purpose address registries)
// that a webhook may never reach.
var (
	deniedV4 = mustPrefixes(
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // CGNAT; cloud metadata (Alibaba Cloud 100.100.100.200)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local; cloud metadata (169.254.169.254)
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, includes broadcast 255.255.255.255
	)
	deniedV6 = mustPrefixes(
		"::/96",          // unspecified, loopback, IPv4-compatible (deprecated)
		"64:ff9b:1::/48", // NAT64 local-use: the embedded IPv4 position varies, deny
		"100::/64",       // discard-only
		"2001::/23",      // IETF protocol assignments (Teredo, ORCHID, ...)
		"2001:db8::/32",  // documentation
		"fc00::/7",       // unique local
		"fe80::/10",      // link-local
		"fec0::/10",      // site-local (deprecated)
		"ff00::/8",       // multicast
	)
	nat64WKP      = netip.MustParsePrefix("64:ff9b::/96") // NAT64 well-known prefix: IPv4 in the last 32 bits
	sixToFour     = netip.MustParsePrefix("2002::/16")    // 6to4: IPv4 in bits 16-47
	globalUnicast = netip.MustParsePrefix("2000::/3")
)

// PublicIP reports whether a webhook may connect to ip: it must not be in a
// special-purpose range. IPv4-mapped, NAT64 (64:ff9b::/96) and 6to4 addresses
// are judged by the IPv4 address they embed; any other IPv6 address must be
// global unicast (2000::/3) outside the denied IPv6 ranges.
func PublicIP(ip netip.Addr) bool {
	ip = ip.WithZone("")
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if ip.Is4() {
		for _, p := range deniedV4 {
			if p.Contains(ip) {
				return false
			}
		}
		return true
	}
	if !ip.Is6() {
		return false
	}
	b := ip.As16()
	switch {
	case nat64WKP.Contains(ip):
		return PublicIP(netip.AddrFrom4([4]byte(b[12:16])))
	case sixToFour.Contains(ip):
		return PublicIP(netip.AddrFrom4([4]byte(b[2:6])))
	case !globalUnicast.Contains(ip):
		return false
	}
	for _, p := range deniedV6 {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// NewWebhookClient returns a client that does not follow redirects and, unless
// allowPrivate, refuses to connect to any address that is not PublicIP
// (checked on the resolved IP at dial time, so DNS rebinding cannot bypass
// it). Build it once and reuse it: every client owns a connection pool.
func NewWebhookClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip, err := netip.ParseAddr(host); err != nil || !PublicIP(ip) {
				return fmt.Errorf("webhook target %s is not a public address", host)
			}
			return nil
		}
	}
	tr := &http.Transport{DialContext: d.DialContext, Proxy: nil, MaxIdleConns: 16, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (k *Kit) httpCreateWebhook(w http.ResponseWriter, r *http.Request, a actor) error {
	var req struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	h, err := k.CreateWebhook(r.Context(), a.Tenant, req.URL, req.Events)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, h)
	return nil
}

func (k *Kit) httpListWebhooks(w http.ResponseWriter, r *http.Request, a actor) error {
	hs, err := k.ListWebhooks(r.Context(), a.Tenant)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"webhooks": hs})
	return nil
}

func (k *Kit) httpDisableWebhook(w http.ResponseWriter, r *http.Request, a actor) error {
	if err := k.DisableWebhook(r.Context(), a.Tenant, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (k *Kit) httpDeliveries(w http.ResponseWriter, r *http.Request, a actor) error {
	ds, err := k.Deliveries(r.Context(), a.Tenant, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deliveries": ds})
	return nil
}
