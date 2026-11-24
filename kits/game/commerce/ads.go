package commerce

import (
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
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
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
)

// AdReward is a verified rewarded-ad callback.
type AdReward struct {
	EventID string // network transaction id, unique per network
	Token   string // intent token issued by POST /ads/{placement}/intent
}

// AdVerifier authenticates a server-to-server reward callback. rawQuery is the
// undecoded query string, body the request body (POST).
type AdVerifier interface {
	VerifyReward(ctx context.Context, gameID, rawQuery string, body []byte) (*AdReward, error)
}

func badAdSignature(msg string) error {
	return httpx.NewError(http.StatusUnauthorized, "invalid_signature", msg)
}

// --- AdMob server-side verification -----------------------------------------

// AdMobKeyURL is Google's published SSV verifier key set.
const AdMobKeyURL = "https://www.gstatic.com/admob/reward/verifier-keys.json"

// KeySource returns the ECDSA public key for an AdMob key_id.
type KeySource interface {
	Key(ctx context.Context, keyID string) (*ecdsa.PublicKey, error)
}

// AdMobVerifier verifies AdMob SSV callbacks: an ECDSA/SHA-256 signature over
// the query string up to "&signature=", with key_id selecting the key.
type AdMobVerifier struct {
	Keys KeySource
}

func (v *AdMobVerifier) VerifyReward(ctx context.Context, _ string, rawQuery string, _ []byte) (*AdReward, error) {
	i := strings.Index(rawQuery, "&signature=")
	if i < 0 {
		return nil, badAdSignature("missing signature")
	}
	msg := rawQuery[:i]
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, badAdSignature("malformed query")
	}
	keyID, sigS := q.Get("key_id"), q.Get("signature")
	if keyID == "" || sigS == "" {
		return nil, badAdSignature("missing key_id or signature")
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(sigS, "="))
	if err != nil {
		return nil, badAdSignature("malformed signature")
	}
	pub, err := v.Keys.Key(ctx, keyID)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256([]byte(msg))
	if !ecdsa.VerifyASN1(pub, h[:], sig) {
		return nil, badAdSignature("signature does not verify")
	}
	// Only parameters inside the signed prefix are trusted.
	signed, _ := url.ParseQuery(msg)
	tok := signed.Get("custom_data")
	if tok == "" {
		tok = signed.Get("user_id")
	}
	ev := signed.Get("transaction_id")
	if ev == "" || tok == "" {
		return nil, httpx.BadRequest("transaction_id and custom_data (or user_id) are required")
	}
	return &AdReward{EventID: ev, Token: tok}, nil
}

// AdMobKeyFetcher fetches and caches Google's verifier keys. Fetches happen in
// the request before any database transaction; an unknown key_id triggers a
// refetch at most once per MinRefresh.
type AdMobKeyFetcher struct {
	URL        string        // default AdMobKeyURL
	Client     *http.Client  // default 5s timeout client
	TTL        time.Duration // default 24h
	MinRefresh time.Duration // default 1m

	mu          sync.Mutex
	keys        map[string]*ecdsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

func (f *AdMobKeyFetcher) Key(ctx context.Context, keyID string) (*ecdsa.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ttl, minRefresh := f.TTL, f.MinRefresh
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	if minRefresh == 0 {
		minRefresh = time.Minute
	}
	k, ok := f.keys[keyID]
	fresh := time.Since(f.fetchedAt) < ttl
	if ok && fresh {
		return k, nil
	}
	if time.Since(f.lastAttempt) >= minRefresh || f.lastAttempt.IsZero() {
		f.lastAttempt = time.Now()
		if keys, err := f.fetch(ctx); err == nil {
			f.keys, f.fetchedAt = keys, time.Now()
		} else if !ok {
			return nil, httpx.NewError(http.StatusServiceUnavailable, "key_fetch_failed", "ad verifier keys unavailable")
		}
	}
	if k, ok := f.keys[keyID]; ok {
		return k, nil
	}
	return nil, badAdSignature("unknown key_id")
}

func (f *AdMobKeyFetcher) fetch(ctx context.Context) (map[string]*ecdsa.PublicKey, error) {
	u, c := f.URL, f.Client
	if u == "" {
		u = AdMobKeyURL
	}
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admob keys: status %d", res.StatusCode)
	}
	var doc struct {
		Keys []struct {
			KeyID json.Number `json:"keyId"`
			PEM   string      `json:"pem"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, err
	}
	out := map[string]*ecdsa.PublicKey{}
	for _, k := range doc.Keys {
		b, _ := pem.Decode([]byte(k.PEM))
		if b == nil {
			continue
		}
		pk, err := x509.ParsePKIXPublicKey(b.Bytes)
		if err != nil {
			continue
		}
		if ec, ok := pk.(*ecdsa.PublicKey); ok {
			out[k.KeyID.String()] = ec
		}
	}
	if len(out) == 0 {
		return nil, errors.New("admob keys: no usable keys")
	}
	return out, nil
}

// --- generic HMAC callbacks -------------------------------------------------

// HMACAdVerifier verifies callbacks signed with a shared secret:
// parameters event_id, token, ts (unix seconds) and sig, where
// sig = hex(HMAC-SHA256(secret, canonical)) and canonical is every parameter
// except sig, sorted by name, joined as name=value with "&" (values decoded).
// Parameters come from the query string, or from a form-encoded POST body.
type HMACAdVerifier struct {
	Secrets map[string][]byte // by game_id
	MaxSkew time.Duration     // default 1h
	Now     func() time.Time
}

func (v *HMACAdVerifier) VerifyReward(_ context.Context, gameID, rawQuery string, body []byte) (*AdReward, error) {
	secret, ok := v.Secrets[gameID]
	if !ok || len(secret) == 0 {
		return nil, errNotConfigured
	}
	raw := rawQuery
	if raw == "" {
		raw = string(body)
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, badAdSignature("malformed parameters")
	}
	sig, err := hex.DecodeString(q.Get("sig"))
	if err != nil || len(sig) != sha256.Size {
		return nil, badAdSignature("missing or malformed sig")
	}
	names := make([]string, 0, len(q))
	for k, vs := range q {
		if len(vs) != 1 {
			return nil, badAdSignature("repeated parameter " + k)
		}
		if k != "sig" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k + "=" + q.Get(k)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strings.Join(parts, "&")))
	if !hmac.Equal(mac.Sum(nil), sig) {
		return nil, badAdSignature("signature does not verify")
	}
	ts, err := strconv.ParseInt(q.Get("ts"), 10, 64)
	if err != nil {
		return nil, httpx.BadRequest("ts is required")
	}
	now, skew := time.Now(), v.MaxSkew
	if v.Now != nil {
		now = v.Now()
	}
	if skew == 0 {
		skew = time.Hour
	}
	if d := now.Sub(time.Unix(ts, 0)); d > skew || d < -skew {
		return nil, badAdSignature("callback timestamp outside the accepted window")
	}
	if q.Get("event_id") == "" || q.Get("token") == "" {
		return nil, httpx.BadRequest("event_id and token are required")
	}
	return &AdReward{EventID: q.Get("event_id"), Token: q.Get("token")}, nil
}

// --- routes -----------------------------------------------------------------

const maxOpenIntents = 20

func (m *Module) intentTTL() time.Duration {
	if m.AdIntentTTL > 0 {
		return m.AdIntentTTL
	}
	return 24 * time.Hour
}

type placement struct {
	Grants   grants
	DailyCap int
}

func loadPlacement(ctx context.Context, tx *sql.Tx, gameID, id string) (*placement, error) {
	var g string
	var p placement
	err := tx.QueryRowContext(ctx, `SELECT grants, daily_cap FROM commerce_ad_placements WHERE game_id=? AND placement_id=?`, gameID, id).Scan(&g, &p.DailyCap)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	p.Grants = parseGrants(g)
	return &p, err
}

func rewardsToday(ctx context.Context, tx *sql.Tx, gameID, playerID, placementID, day string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM commerce_ad_rewards WHERE game_id=? AND player_id=? AND placement_id=? AND day=?`,
		gameID, playerID, placementID, day).Scan(&n)
	return n, err
}

// adIntent issues a single-use opaque token to pass to the ad SDK as
// custom_data/user_id; the reward callback is credited to this player only.
func (m *Module) adIntent(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	pl := r.PathValue("placement")
	if !game.ValidID(pl) {
		return httpx.NotFound("placement not found")
	}
	now := m.now()
	tok := strings.ReplaceAll(httpx.RandomID("adt_"), "-", "")
	exp := now.Add(m.intentTTL()).UnixMilli()
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		p, err := loadPlacement(ctx, tx, s.GameID, pl)
		if err != nil {
			return err
		}
		if p == nil {
			return httpx.NotFound("placement not found")
		}
		n, err := rewardsToday(ctx, tx, s.GameID, s.PlayerID, pl, game.SeasonID("daily", now))
		if err != nil {
			return err
		}
		if n >= p.DailyCap {
			return httpx.Conflict("ad_cap_reached", "daily ad reward limit reached")
		}
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM commerce_ad_intents WHERE game_id=? AND player_id=? AND used_at IS NULL AND expires_at>?`,
			s.GameID, s.PlayerID, now.UnixMilli()).Scan(&open); err != nil {
			return err
		}
		if open >= maxOpenIntents {
			return httpx.Conflict("too_many_intents", "too many unused ad intents")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO commerce_ad_intents(token,game_id,player_id,placement_id,created_at,expires_at) VALUES(?,?,?,?,?,?)`,
			tok, s.GameID, s.PlayerID, pl, now.UnixMilli(), exp)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"token": tok, "placement_id": pl, "expires_at": exp})
	return nil
}

// adCallback credits a verified reward callback (idempotent per network event).
// Verified callbacks that cannot be credited (unknown/expired/used token, cap
// reached) are answered 200 with status "ignored" so the network stops retrying.
func (m *Module) adCallback(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	provider := r.PathValue("provider")
	v, ok := m.Ads[provider]
	if !ok {
		return errNotConfigured
	}
	g := game.CurrentGame(ctx)
	var body []byte
	if r.Method == http.MethodPost {
		var err error
		if body, err = game.ReadBody(r, 16<<10); err != nil {
			return err
		}
	}
	if len(r.URL.RawQuery) > 16<<10 {
		return httpx.TooLarge("query too large")
	}
	rw, err := v.VerifyReward(ctx, g.ID, r.URL.RawQuery, body)
	if err != nil {
		return err
	}
	if len(rw.EventID) > 128 || len(rw.Token) > 128 {
		return httpx.BadRequest("event id or token too long")
	}
	res, err := m.processEvent(ctx, g.ID, "ad:"+provider, rw.EventID, "ad_reward", hashOf(append([]byte(r.URL.RawQuery+"\n"), body...)), func(tx *sql.Tx) (inboxState, error) {
		return m.creditAd(ctx, tx, g, provider, rw)
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, res)
	return nil
}

func (m *Module) creditAd(ctx context.Context, tx *sql.Tx, g game.Game, provider string, rw *AdReward) (inboxState, error) {
	now := m.now()
	ms := now.UnixMilli()
	var player, pl string
	var exp int64
	var used sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT player_id, placement_id, expires_at, used_at FROM commerce_ad_intents WHERE token=? AND game_id=?`,
		rw.Token, g.ID).Scan(&player, &pl, &exp, &used)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return inboxState{Status: "ignored", Error: "unknown_token"}, nil
	case err != nil:
		return inboxState{}, err
	case used.Valid:
		return inboxState{Status: "ignored", Error: "token_used"}, nil
	case exp <= ms:
		return inboxState{Status: "ignored", Error: "token_expired"}, nil
	}
	p, err := loadPlacement(ctx, tx, g.ID, pl)
	if err != nil {
		return inboxState{}, err
	}
	if p == nil {
		return inboxState{Status: "ignored", Error: "placement_removed"}, nil
	}
	day := game.SeasonID("daily", now)
	n, err := rewardsToday(ctx, tx, g.ID, player, pl, day)
	if err != nil {
		return inboxState{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commerce_ad_intents SET used_at=? WHERE token=?`, ms, rw.Token); err != nil {
		return inboxState{}, err
	}
	if n >= p.DailyCap {
		return inboxState{Status: "ignored", Error: "daily_cap_reached"}, nil
	}
	// UNIQUE(game,player,placement,day,seq) is the final guard on the cap.
	if _, err := tx.ExecContext(ctx, `INSERT INTO commerce_ad_rewards(game_id,provider,event_id,player_id,placement_id,day,seq,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		g.ID, provider, rw.EventID, player, pl, day, n+1, ms); err != nil {
		return inboxState{}, err
	}
	if err := game.ApplyLedger(ctx, tx, g.ID, player, "ad", "ad:"+provider+":"+rw.EventID, p.Grants, ms); err != nil {
		return inboxState{}, err
	}
	if err := m.enqueue(ctx, tx, g.TenantID, "ad:"+provider+":"+rw.EventID, OutboxEvent{Event: "ad_reward", GameID: g.ID, PlayerID: player, PlacementID: pl, Grants: p.Grants, At: ms}); err != nil {
		return inboxState{}, err
	}
	return inboxState{Status: "processed"}, nil
}
