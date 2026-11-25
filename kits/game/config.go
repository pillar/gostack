package game

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
)

// Document is a published game configuration (GAME_API.md "配置").
type Document struct {
	Client   map[string]json.RawMessage `json:"client"`
	Boards   map[string]BoardDef        `json:"boards"`
	Rewards  map[string]RewardDef       `json:"rewards"`
	Saves    *SavesDef                  `json:"saves,omitempty"`
	Items    map[string]ItemDef         `json:"items,omitempty"`
	Offers   map[string]OfferDef        `json:"offers,omitempty"`
	Rollouts []RolloutDef               `json:"rollouts,omitempty"`
}

type BoardDef struct {
	Order           string `json:"order"`  // desc | asc
	Mode            string `json:"mode"`   // best | sum
	Period          string `json:"period"` // none | daily | weekly
	MinScore        int64  `json:"min_score"`
	MaxScore        int64  `json:"max_score"`
	QuarantineAbove *int64 `json:"quarantine_above,omitempty"`
	// RuleVersion, when > 0, must be echoed by every submission.
	RuleVersion int64 `json:"rule_version,omitempty"`
	// RequireRun demands a one-time run token from POST /boards/{board}/runs.
	RequireRun bool  `json:"require_run,omitempty"`
	MinRunMs   int64 `json:"min_run_ms,omitempty"` // shorter runs are quarantined
	RunTTLMs   int64 `json:"run_ttl_ms,omitempty"` // 0 = defaultRunTTL
	// Settlement pays rank rewards when a daily/weekly season ends.
	Settlement *SettlementDef `json:"settlement,omitempty"`
}

type SettlementDef struct {
	Rewards []RankReward `json:"rewards"`
}

type RankReward struct {
	FromRank int64            `json:"from_rank"`
	ToRank   int64            `json:"to_rank"`
	Grants   map[string]int64 `json:"grants"`
}

type RewardDef struct {
	Grants    map[string]int64 `json:"grants"`
	Period    string           `json:"period"` // once | daily
	PerPeriod int              `json:"per_period"`
	StartsAt  int64            `json:"starts_at"` // 0 = unbounded
	EndsAt    int64            `json:"ends_at"`   // 0 = unbounded
}

// SavesDef holds save compatibility rules.
type SavesDef struct {
	MinSchemaVersion int64 `json:"min_schema_version"`
}

// ItemDef is a catalog entry; assets are currencies or stackable items.
type ItemDef struct {
	Kind     string `json:"kind"`                // currency | item
	Name     string `json:"name,omitempty"`      // display name
	MaxStack int64  `json:"max_stack,omitempty"` // items only; 0 = unlimited
}

// OfferDef is a fixed-price exchange of assets.
type OfferDef struct {
	Cost      map[string]int64 `json:"cost"`
	Grants    map[string]int64 `json:"grants"`
	Period    string           `json:"period"`               // once | daily | unlimited
	PerPeriod int              `json:"per_period,omitempty"` // required unless unlimited
}

// RolloutDef overrides client keys for a targeted, percentage-bucketed audience.
type RolloutDef struct {
	ID               string                     `json:"id"`
	Enabled          bool                       `json:"enabled"`
	Percent          int                        `json:"percent"`
	Platforms        []string                   `json:"platforms,omitempty"`
	MinClientVersion string                     `json:"min_client_version,omitempty"`
	MaxClientVersion string                     `json:"max_client_version,omitempty"`
	Client           map[string]json.RawMessage `json:"client"`
}

const (
	maxGrant      = 1e12
	maxSettleRank = 10000
	defaultRunTTL = int64(time.Hour / time.Millisecond)
	maxRunTTL     = int64(7 * 24 * time.Hour / time.Millisecond)
)

// ParseDocument strictly parses and validates a configuration document.
func ParseDocument(b []byte) (Document, error) {
	var d Document
	if err := decodeStrict(b, &d); err != nil {
		return d, err
	}
	if d.Client == nil {
		d.Client = map[string]json.RawMessage{}
	}
	for id, bd := range d.Boards {
		if !idPattern.MatchString(id) {
			return d, httpx.BadRequest("invalid board id " + strconv.Quote(id))
		}
		if bd.Order != "desc" && bd.Order != "asc" {
			return d, httpx.BadRequest("board " + id + ": order must be desc or asc")
		}
		if bd.Mode != "best" && bd.Mode != "sum" {
			return d, httpx.BadRequest("board " + id + ": mode must be best or sum")
		}
		if bd.Mode == "sum" && (bd.Order != "desc" || bd.MinScore < 0) {
			return d, httpx.BadRequest("board " + id + ": sum boards must be desc with min_score >= 0")
		}
		if bd.Period != "none" && bd.Period != "daily" && bd.Period != "weekly" {
			return d, httpx.BadRequest("board " + id + ": period must be none, daily or weekly")
		}
		if bd.MinScore > bd.MaxScore {
			return d, httpx.BadRequest("board " + id + ": min_score > max_score")
		}
	}
	for id, rd := range d.Rewards {
		if !idPattern.MatchString(id) {
			return d, httpx.BadRequest("invalid reward id " + strconv.Quote(id))
		}
		if err := d.checkAssets("reward "+id+": grants", rd.Grants); err != nil {
			return d, err
		}
		if rd.Period != "once" && rd.Period != "daily" {
			return d, httpx.BadRequest("reward " + id + ": period must be once or daily")
		}
		if rd.PerPeriod < 1 || rd.PerPeriod > 1000 {
			return d, httpx.BadRequest("reward " + id + ": per_period must be within [1,1000]")
		}
	}
	if err := d.validateP0(); err != nil {
		return d, err
	}
	return d, nil
}

// validateP0 checks the optional sections added after the first contract.
func (d *Document) validateP0() error {
	for id, it := range d.Items {
		if !idPattern.MatchString(id) {
			return httpx.BadRequest("invalid item id " + strconv.Quote(id))
		}
		if it.Kind != "currency" && it.Kind != "item" {
			return httpx.BadRequest("item " + id + ": kind must be currency or item")
		}
		if it.MaxStack < 0 || (it.Kind == "currency" && it.MaxStack != 0) {
			return httpx.BadRequest("item " + id + ": max_stack must be >= 0 and is only allowed for items")
		}
		if len(it.Name) > 64 {
			return httpx.BadRequest("item " + id + ": name too long")
		}
	}
	for id, bd := range d.Boards {
		if bd.RuleVersion < 0 {
			return httpx.BadRequest("board " + id + ": rule_version must be >= 0")
		}
		if bd.MinRunMs < 0 || bd.RunTTLMs < 0 || bd.RunTTLMs > maxRunTTL {
			return httpx.BadRequest("board " + id + ": min_run_ms/run_ttl_ms out of range")
		}
		if (bd.MinRunMs != 0 || bd.RunTTLMs != 0) && !bd.RequireRun {
			return httpx.BadRequest("board " + id + ": min_run_ms/run_ttl_ms require require_run")
		}
		if bd.MinRunMs >= bd.runTTL() {
			return httpx.BadRequest("board " + id + ": min_run_ms must be below run_ttl_ms")
		}
		if bd.Settlement == nil {
			continue
		}
		if bd.Period != "daily" && bd.Period != "weekly" {
			return httpx.BadRequest("board " + id + ": settlement requires a daily or weekly period")
		}
		if len(bd.Settlement.Rewards) == 0 {
			return httpx.BadRequest("board " + id + ": settlement.rewards required")
		}
		var prev int64
		for i, rr := range bd.Settlement.Rewards {
			if rr.FromRank < 1 || rr.ToRank < rr.FromRank || rr.ToRank > maxSettleRank || rr.FromRank <= prev {
				return httpx.BadRequest(fmt.Sprintf("board %s: settlement.rewards[%d] ranks must be ascending, non-overlapping and within [1,%d]", id, i, maxSettleRank))
			}
			prev = rr.ToRank
			if err := d.checkAssets(fmt.Sprintf("board %s: settlement.rewards[%d].grants", id, i), rr.Grants); err != nil {
				return err
			}
		}
	}
	for id, o := range d.Offers {
		if !idPattern.MatchString(id) {
			return httpx.BadRequest("invalid offer id " + strconv.Quote(id))
		}
		if err := d.checkAssets("offer "+id+": cost", o.Cost); err != nil {
			return err
		}
		if err := d.checkAssets("offer "+id+": grants", o.Grants); err != nil {
			return err
		}
		for a := range o.Cost {
			if _, ok := o.Grants[a]; ok {
				return httpx.BadRequest("offer " + id + ": an asset cannot be both cost and grant")
			}
		}
		switch o.Period {
		case "once", "daily":
			if o.PerPeriod < 1 || o.PerPeriod > 1000 {
				return httpx.BadRequest("offer " + id + ": per_period must be within [1,1000]")
			}
		case "unlimited":
			if o.PerPeriod != 0 {
				return httpx.BadRequest("offer " + id + ": per_period is not allowed for unlimited offers")
			}
		default:
			return httpx.BadRequest("offer " + id + ": period must be once, daily or unlimited")
		}
	}
	if d.Saves != nil && d.Saves.MinSchemaVersion < 0 {
		return httpx.BadRequest("saves.min_schema_version must be >= 0")
	}
	seen := map[string]bool{}
	for i, ro := range d.Rollouts {
		where := fmt.Sprintf("rollouts[%d]", i)
		if !idPattern.MatchString(ro.ID) || seen[ro.ID] {
			return httpx.BadRequest(where + ": id must be a unique id")
		}
		seen[ro.ID] = true
		if ro.Percent < 0 || ro.Percent > 100 {
			return httpx.BadRequest(where + ": percent must be within [0,100]")
		}
		for _, p := range ro.Platforms {
			if !idPattern.MatchString(p) {
				return httpx.BadRequest(where + ": invalid platform " + strconv.Quote(p))
			}
		}
		for _, v := range []string{ro.MinClientVersion, ro.MaxClientVersion} {
			if _, ok := parseVersion(v); v != "" && !ok {
				return httpx.BadRequest(where + ": invalid client version " + strconv.Quote(v))
			}
		}
		if ro.MinClientVersion != "" && ro.MaxClientVersion != "" && compareVersions(ro.MinClientVersion, ro.MaxClientVersion) > 0 {
			return httpx.BadRequest(where + ": min_client_version > max_client_version")
		}
		if len(ro.Client) == 0 {
			return httpx.BadRequest(where + ": client overrides required")
		}
	}
	return nil
}

// checkAssets validates a non-empty asset→amount map: ids, positive bounded
// amounts and, when an items catalog exists, membership in it.
func (d *Document) checkAssets(where string, m map[string]int64) error {
	if len(m) == 0 {
		return httpx.BadRequest(where + " required")
	}
	for asset, n := range m {
		if !idPattern.MatchString(asset) || n <= 0 || n > maxGrant {
			return httpx.BadRequest(where + " must map asset ids to positive integers")
		}
		if len(d.Items) > 0 {
			if _, ok := d.Items[asset]; !ok {
				return httpx.BadRequest(where + ": asset " + strconv.Quote(asset) + " is not in the items catalog")
			}
		}
	}
	return nil
}

func (bd BoardDef) runTTL() int64 {
	if bd.RunTTLMs > 0 {
		return bd.RunTTLMs
	}
	return defaultRunTTL
}

type cachedDoc struct {
	Revision int64
	Doc      Document
	Client   []byte
}

// configCache memoises parsed documents by (game, revision); the current
// revision is always read from the database so publishes from the CLI or another
// handler are visible immediately.
type configCache struct {
	mu   sync.Mutex
	docs map[string]*cachedDoc
}

func newConfigCache() *configCache { return &configCache{docs: map[string]*cachedDoc{}} }

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (k *Kit) currentConfig(ctx context.Context, q queryer, gameID string) (*cachedDoc, error) {
	var rev int64
	err := q.QueryRowContext(ctx, `SELECT coalesce(max(revision),0) FROM config_revisions WHERE game_id=?`, gameID).Scan(&rev)
	if err != nil {
		return nil, err
	}
	return k.configAt(ctx, q, gameID, rev)
}

// configAt returns the parsed document of a specific revision (0 = empty document).
func (k *Kit) configAt(ctx context.Context, q queryer, gameID string, rev int64) (*cachedDoc, error) {
	if rev == 0 {
		return &cachedDoc{Client: []byte("{}")}, nil
	}
	key := gameID + "@" + strconv.FormatInt(rev, 10)
	k.configs.mu.Lock()
	c, ok := k.configs.docs[key]
	k.configs.mu.Unlock()
	if ok {
		return c, nil
	}
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT document FROM config_revisions WHERE game_id=? AND revision=?`, gameID, rev).Scan(&raw); err != nil {
		return nil, err
	}
	doc, err := ParseDocument([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("stored config %s invalid: %w", key, err)
	}
	client, err := json.Marshal(doc.Client)
	if err != nil {
		return nil, err
	}
	c = &cachedDoc{Revision: rev, Doc: doc, Client: client}
	k.configs.mu.Lock()
	if len(k.configs.docs) > 1024 {
		clear(k.configs.docs)
	}
	k.configs.docs[key] = c
	k.configs.mu.Unlock()
	return c, nil
}

// CreateGame registers a game (G01). Used by the admin API and CLI.
func CreateGame(ctx context.Context, db *store.DB, tenant, id, name string, origins []string) error {
	if !idPattern.MatchString(id) {
		return httpx.BadRequest("game_id must match [a-z0-9][a-z0-9_-]{0,31}")
	}
	if name == "" {
		name = id
	}
	if origins == nil {
		origins = []string{}
	}
	o, _ := json.Marshal(origins)
	now := time.Now().UnixMilli()
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO games(game_id,tenant_id,name,status,allowed_origins,created_at,updated_at) VALUES(?,?,?,'active',?,?,?)`,
			id, tenant, name, string(o), now, now)
		return err
	})
	if store.IsUniqueViolation(err) {
		return httpx.Conflict("conflict", "game already exists")
	}
	return err
}

// PublishConfig validates and stores doc as the next revision.
func PublishConfig(ctx context.Context, db *store.DB, gameID string, doc []byte, by, note string) (int64, error) {
	if _, err := ParseDocument(doc); err != nil {
		return 0, err
	}
	var rev int64
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM games WHERE game_id=?`, gameID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return httpx.NotFound("game not found")
		}
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(revision),0)+1 FROM config_revisions WHERE game_id=?`, gameID).Scan(&rev); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO config_revisions(game_id,revision,document,published_by,note,created_at) VALUES(?,?,?,?,?,?)`,
			gameID, rev, string(doc), by, note, time.Now().UnixMilli())
		return err
	})
	return rev, err
}

// RollbackConfig republishes an earlier revision's document as a new revision (history stays append-only).
func RollbackConfig(ctx context.Context, db *store.DB, gameID string, to int64, by string) (int64, error) {
	var raw string
	err := db.Write.QueryRowContext(ctx, `SELECT document FROM config_revisions WHERE game_id=? AND revision=?`, gameID, to).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, httpx.NotFound("revision not found")
	}
	if err != nil {
		return 0, err
	}
	return PublishConfig(ctx, db, gameID, []byte(raw), by, "rollback to "+strconv.FormatInt(to, 10))
}

func (k *Kit) adminGame(r *http.Request) (gameRow, auth.APIKey, error) {
	key := r.Context().Value(adminKey).(auth.APIKey)
	g, err := k.loadGame(r.Context(), k.DB.Read, r.PathValue("game"))
	if err != nil {
		return g, key, err
	}
	if g.TenantID != key.TenantID {
		return g, key, httpx.NotFound("game not found") // do not reveal other tenants' games
	}
	return g, key, nil
}

func (k *Kit) adminCreateGame(w http.ResponseWriter, r *http.Request) error {
	key := r.Context().Value(adminKey).(auth.APIKey)
	var req struct {
		GameID         string   `json:"game_id"`
		Name           string   `json:"name"`
		AllowedOrigins []string `json:"allowed_origins"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if err := CreateGame(r.Context(), k.DB, key.TenantID, req.GameID, req.Name, req.AllowedOrigins); err != nil {
		return mapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]string{"game_id": req.GameID})
	return nil
}

func (k *Kit) adminPublishConfig(w http.ResponseWriter, r *http.Request) error {
	g, key, err := k.adminGame(r)
	if err != nil {
		return err
	}
	body, err := readBody(r, 1<<20)
	if err != nil {
		return err
	}
	rev, err := PublishConfig(r.Context(), k.DB, g.ID, body, key.ID, "")
	if err != nil {
		return mapWriteErr(err)
	}
	k.Log.InfoContext(r.Context(), "config published", "game", g.ID, "revision", rev, "by", key.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"revision": rev})
	return nil
}

func (k *Kit) adminRollbackConfig(w http.ResponseWriter, r *http.Request) error {
	g, key, err := k.adminGame(r)
	if err != nil {
		return err
	}
	var req struct {
		Revision int64 `json:"revision"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	rev, err := RollbackConfig(r.Context(), k.DB, g.ID, req.Revision, key.ID)
	if err != nil {
		return mapWriteErr(err)
	}
	k.Log.InfoContext(r.Context(), "config rolled back", "game", g.ID, "to", req.Revision, "revision", rev, "by", key.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"revision": rev})
	return nil
}

// mapWriteErr converts write-queue saturation into 503 overloaded.
func mapWriteErr(err error) error {
	if errors.Is(err, store.ErrOverloaded) {
		return httpx.Overloaded("write queue full")
	}
	return err
}
