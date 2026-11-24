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

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
)

// Document is a published game configuration (GAME_API.md "配置").
type Document struct {
	Client  map[string]json.RawMessage `json:"client"`
	Boards  map[string]BoardDef        `json:"boards"`
	Rewards map[string]RewardDef       `json:"rewards"`
}

type BoardDef struct {
	Order           string `json:"order"`  // desc | asc
	Mode            string `json:"mode"`   // best | sum
	Period          string `json:"period"` // none | daily | weekly
	MinScore        int64  `json:"min_score"`
	MaxScore        int64  `json:"max_score"`
	QuarantineAbove *int64 `json:"quarantine_above,omitempty"`
}

type RewardDef struct {
	Grants    map[string]int64 `json:"grants"`
	Period    string           `json:"period"` // once | daily
	PerPeriod int              `json:"per_period"`
	StartsAt  int64            `json:"starts_at"` // 0 = unbounded
	EndsAt    int64            `json:"ends_at"`   // 0 = unbounded
}

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
		if len(rd.Grants) == 0 {
			return d, httpx.BadRequest("reward " + id + ": grants required")
		}
		for asset, n := range rd.Grants {
			if !idPattern.MatchString(asset) || n <= 0 || n > 1e12 {
				return d, httpx.BadRequest("reward " + id + ": grants must map asset ids to positive integers")
			}
		}
		if rd.Period != "once" && rd.Period != "daily" {
			return d, httpx.BadRequest("reward " + id + ": period must be once or daily")
		}
		if rd.PerPeriod < 1 || rd.PerPeriod > 1000 {
			return d, httpx.BadRequest("reward " + id + ": per_period must be within [1,1000]")
		}
	}
	return d, nil
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

func (k *Kit) getConfig(w http.ResponseWriter, r *http.Request) error {
	c, err := k.currentConfig(r.Context(), k.DB.Read, gameFrom(r.Context()).ID)
	if err != nil {
		return err
	}
	etag := `"` + strconv.FormatInt(c.Revision, 10) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"revision":%d,"data":%s}`+"\n", c.Revision, c.Client)
	return nil
}

// CreateGame registers a game (G01). Used by the admin API and CLI.
func CreateGame(ctx context.Context, db *sqlite.DB, tenant, id, name string, origins []string) error {
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
	if sqlite.IsUniqueViolation(err) {
		return httpx.Conflict("conflict", "game already exists")
	}
	return err
}

// PublishConfig validates and stores doc as the next revision.
func PublishConfig(ctx context.Context, db *sqlite.DB, gameID string, doc []byte, by, note string) (int64, error) {
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
func RollbackConfig(ctx context.Context, db *sqlite.DB, gameID string, to int64, by string) (int64, error) {
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
	if errors.Is(err, sqlite.ErrOverloaded) {
		return httpx.Overloaded("write queue full")
	}
	return err
}
