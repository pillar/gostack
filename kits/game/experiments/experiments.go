// Package experiments implements GameKit experiments (G12): stable bucketing,
// mutually exclusive layers, exposure recording and a kill switch.
//
// Bucketing: Bucket(game, salt, player, n) = uint64(first 8 bytes of
// SHA-256(game 0x00 salt 0x00 player)) mod n. It is a pure function, so
// assignments are stable across calls, restarts and servers.
//
// Layers: every layer has 10000 slots. A player's slot in a layer is
// Bucket(game, "layer:"+layer, player, 10000). A running experiment owns the
// contiguous slice [slice_start, slice_start + traffic_percent*100) of its layer;
// slices of running experiments never overlap (checked when an experiment starts
// or grows), so a player is in at most one running experiment per layer. The
// slice is fixed at start, so stopping or editing another experiment never moves
// players. Inside its slice a player gets a variant by weight:
// Bucket(game, "exp:"+id, player, sum of weights) walked over the variants in order.
package experiments

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
)

const layerSlots = 10000

// Module wires the experiments feature package.
type Module struct {
	Kit *game.Kit
	DB  *sqlite.DB
	Now func() time.Time // defaults to time.Now
}

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Routes registers the client and admin routes.
func (m *Module) Routes(mux *http.ServeMux) {
	k := m.Kit
	k.Handle(mux, "GET /experiments", game.Player, m.getAssignments)
	k.Handle(mux, "POST /experiments/exposures", game.Player, m.postExposures)
	k.HandleAdmin(mux, "GET /experiments", m.adminList)
	k.HandleAdmin(mux, "GET /experiments/{id}", m.adminGet)
	k.HandleAdmin(mux, "PUT /experiments/{id}", m.adminPut)
	k.HandleAdmin(mux, "POST /experiments/{id}/stop", m.adminStop)
	k.HandleAdmin(mux, "DELETE /experiments/{id}", m.adminDelete)
	k.HandleAdmin(mux, "GET /experiments/{id}/results", m.adminResults)
}

// Maintain has nothing to clean up: exposures are kept for analysis.
func (m *Module) Maintain(ctx context.Context) error { return nil }

// ---- model and pure bucketing ----

// Variant is one arm of an experiment.
type Variant struct {
	ID     string          `json:"id"`
	Weight int             `json:"weight"`
	Params json.RawMessage `json:"params"`
}

// Experiment is the stored definition.
type Experiment struct {
	ID               string    `json:"id"`
	Layer            string    `json:"layer"`
	Status           string    `json:"status"`
	Variants         []Variant `json:"variants"`
	Control          string    `json:"control"`
	TrafficPercent   int       `json:"traffic_percent"`
	Platforms        []string  `json:"platforms"`
	MinClientVersion string    `json:"min_client_version"`
	SliceStart       int       `json:"-"`
}

// Bucket maps (game, salt, player) deterministically into [0, n).
func Bucket(gameID, salt, playerID string, n uint64) uint64 {
	h := sha256.New()
	h.Write([]byte(gameID))
	h.Write([]byte{0})
	h.Write([]byte(salt))
	h.Write([]byte{0})
	h.Write([]byte(playerID))
	return binary.BigEndian.Uint64(h.Sum(nil)[:8]) % n
}

// LayerSlot is the player's slot (0..9999) in a layer.
func LayerSlot(gameID, layer, playerID string) int {
	return int(Bucket(gameID, "layer:"+layer, playerID, layerSlots))
}

// PickVariant chooses a variant by weight, independent of the layer slot.
func PickVariant(gameID, experimentID, playerID string, variants []Variant) Variant {
	total := 0
	for _, v := range variants {
		total += v.Weight
	}
	x := int(Bucket(gameID, "exp:"+experimentID, playerID, uint64(total)))
	for _, v := range variants {
		if x < v.Weight {
			return v
		}
		x -= v.Weight
	}
	return variants[len(variants)-1]
}

// Assignment is the outcome for one experiment.
type Assignment struct {
	Experiment string          `json:"-"`
	Layer      string          `json:"layer"`
	Variant    string          `json:"variant"`
	Params     json.RawMessage `json:"params"`
	Stopped    bool            `json:"stopped,omitempty"`
}

func (e Experiment) eligible(platform, clientVersion string) bool {
	if len(e.Platforms) > 0 {
		ok := false
		for _, p := range e.Platforms {
			ok = ok || p == platform
		}
		if !ok {
			return false
		}
	}
	return e.MinClientVersion == "" || (clientVersion != "" && compareVersions(clientVersion, e.MinClientVersion) >= 0)
}

// Assign computes the player's assignment for one experiment; ok is false when
// the player is not part of it (draft, outside the traffic slice, or ineligible).
// A stopped experiment returns its control variant for everyone (kill switch).
func Assign(gameID, playerID string, e Experiment, platform, clientVersion string) (Assignment, bool) {
	switch e.Status {
	case "stopped":
		for _, v := range e.Variants {
			if v.ID == e.Control {
				return Assignment{e.ID, e.Layer, v.ID, v.Params, true}, true
			}
		}
	case "running":
		if !e.eligible(platform, clientVersion) {
			return Assignment{}, false
		}
		slot := LayerSlot(gameID, e.Layer, playerID)
		if slot < e.SliceStart || slot >= e.SliceStart+e.TrafficPercent*100 {
			return Assignment{}, false
		}
		v := PickVariant(gameID, e.ID, playerID, e.Variants)
		return Assignment{e.ID, e.Layer, v.ID, v.Params, false}, true
	}
	return Assignment{}, false
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ---- storage ----

const expCols = `id,layer,status,variants,control,traffic_percent,platforms,min_client_version,slice_start`

func scanExp(sc interface{ Scan(...any) error }) (Experiment, error) {
	var e Experiment
	var variants, platforms string
	if err := sc.Scan(&e.ID, &e.Layer, &e.Status, &variants, &e.Control, &e.TrafficPercent, &platforms, &e.MinClientVersion, &e.SliceStart); err != nil {
		return e, err
	}
	if err := json.Unmarshal([]byte(variants), &e.Variants); err != nil {
		return e, err
	}
	_ = json.Unmarshal([]byte(platforms), &e.Platforms)
	if e.Platforms == nil {
		e.Platforms = []string{}
	}
	return e, nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func listExperiments(ctx context.Context, q queryer, gameID string) ([]Experiment, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+expCols+` FROM exp_experiments WHERE game_id=? ORDER BY id LIMIT 500`, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Experiment{}
	for rows.Next() {
		e, err := scanExp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func getExperiment(ctx context.Context, q queryer, gameID, id string) (Experiment, error) {
	e, err := scanExp(q.QueryRowContext(ctx, `SELECT `+expCols+` FROM exp_experiments WHERE game_id=? AND id=?`, gameID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, httpx.NotFound("experiment not found")
	}
	return e, err
}

func (e Experiment) adminJSON() map[string]any {
	m := map[string]any{"id": e.ID, "layer": e.Layer, "status": e.Status, "variants": e.Variants, "control": e.Control,
		"traffic_percent": e.TrafficPercent, "platforms": e.Platforms, "min_client_version": e.MinClientVersion}
	if e.Status == "running" {
		m["slice_start"] = e.SliceStart
		m["slice_end"] = e.SliceStart + e.TrafficPercent*100
	}
	return m
}

// ---- client ----

func (m *Module) assignments(ctx context.Context, q queryer, gameID, playerID, platform, version string) (map[string]Assignment, error) {
	exps, err := listExperiments(ctx, q, gameID)
	if err != nil {
		return nil, err
	}
	out := map[string]Assignment{}
	for _, e := range exps {
		if a, ok := Assign(gameID, playerID, e, platform, version); ok {
			out[e.ID] = a
		}
	}
	return out, nil
}

func (m *Module) getAssignments(w http.ResponseWriter, r *http.Request) error {
	s := game.CurrentSession(r.Context())
	q := r.URL.Query()
	as, err := m.assignments(r.Context(), m.DB.Read, s.GameID, s.PlayerID, q.Get("platform"), q.Get("client_version"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"assignments": as})
	return nil
}

// postExposures records the first exposure per (player, experiment). Experiments
// the player is not assigned to (unknown, draft, outside traffic, stopped) are
// ignored silently.
func (m *Module) postExposures(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	var req struct {
		ExperimentIDs []string `json:"experiment_ids"`
	}
	b, err := game.ReadBody(r, 8<<10)
	if err != nil {
		return err
	}
	if err := game.DecodeStrict(b, &req); err != nil {
		return err
	}
	if len(req.ExperimentIDs) == 0 || len(req.ExperimentIDs) > 50 {
		return httpx.BadRequest("experiment_ids must contain 1-50 ids")
	}
	q := r.URL.Query()
	platform, version := q.Get("platform"), q.Get("client_version")
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		now := m.now().UnixMilli()
		for _, id := range req.ExperimentIDs {
			if !game.ValidID(id) {
				continue
			}
			e, err := getExperiment(ctx, tx, s.GameID, id)
			if err != nil {
				var he *httpx.Error
				if errors.As(err, &he) && he.Status == http.StatusNotFound {
					continue
				}
				return err
			}
			a, ok := Assign(s.GameID, s.PlayerID, e, platform, version)
			if !ok || a.Stopped {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO exp_exposures(game_id,experiment_id,player_id,variant,created_at) VALUES(?,?,?,?,?)`,
				s.GameID, id, s.PlayerID, a.Variant, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- admin ----

var versionPattern = regexp.MustCompile(`^\d+(\.\d+){0,3}$`)

type putReq struct {
	Layer            string    `json:"layer"`
	Status           string    `json:"status"`
	Variants         []Variant `json:"variants"`
	Control          string    `json:"control"`
	TrafficPercent   *int      `json:"traffic_percent"`
	Platforms        []string  `json:"platforms"`
	MinClientVersion string    `json:"min_client_version"`
}

func (r putReq) validate() error {
	if !game.ValidID(r.Layer) {
		return httpx.BadRequest("layer must match [a-z0-9][a-z0-9_-]{0,31}")
	}
	if r.Status == "" {
		return httpx.BadRequest("status is required (draft, running or stopped)")
	}
	if r.Status != "draft" && r.Status != "running" && r.Status != "stopped" {
		return httpx.BadRequest("status must be draft, running or stopped")
	}
	if len(r.Variants) == 0 || len(r.Variants) > 10 {
		return httpx.BadRequest("variants must contain 1-10 entries")
	}
	seen := map[string]bool{}
	for i, v := range r.Variants {
		if !game.ValidID(v.ID) || seen[v.ID] {
			return httpx.BadRequest("variant ids must be unique and match [a-z0-9][a-z0-9_-]{0,31}")
		}
		seen[v.ID] = true
		if v.Weight < 1 || v.Weight > 10000 {
			return httpx.BadRequest("variant weight must be an integer between 1 and 10000")
		}
		if len(v.Params) == 0 {
			r.Variants[i].Params = json.RawMessage("{}")
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(v.Params, &obj) != nil || obj == nil || len(v.Params) > 4<<10 {
			return httpx.BadRequest("variant params must be a JSON object up to 4KiB")
		}
	}
	if !seen[r.Control] {
		return httpx.BadRequest("control must be one of the variant ids")
	}
	if r.TrafficPercent == nil || *r.TrafficPercent < 0 || *r.TrafficPercent > 100 {
		return httpx.BadRequest("traffic_percent is required and must be an integer between 0 and 100")
	}
	if len(r.Platforms) > 8 {
		return httpx.BadRequest("at most 8 platforms")
	}
	if r.MinClientVersion != "" && !versionPattern.MatchString(r.MinClientVersion) {
		return httpx.BadRequest("min_client_version must be dotted numbers such as 1.2.0")
	}
	return nil
}

func sameVariants(a, b []Variant) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Weight != b[i].Weight || !jsonEqual(a[i].Params, b[i].Params) {
			return false
		}
	}
	return true
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

type span struct{ start, end int }

// runningSpans returns the slices of running experiments in layer except skipID.
func runningSpans(ctx context.Context, q queryer, gameID, layer, skipID string) ([]span, error) {
	exps, err := listExperiments(ctx, q, gameID)
	if err != nil {
		return nil, err
	}
	var out []span
	for _, e := range exps {
		if e.Layer == layer && e.Status == "running" && e.ID != skipID && e.TrafficPercent > 0 {
			out = append(out, span{e.SliceStart, e.SliceStart + e.TrafficPercent*100})
		}
	}
	return out, nil
}

// firstFit finds the lowest start where width slots fit between spans, or -1.
func firstFit(spans []span, width int) int {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	cursor := 0
	for _, s := range spans {
		if s.start-cursor >= width {
			return cursor
		}
		cursor = max(cursor, s.end)
	}
	if layerSlots-cursor >= width {
		return cursor
	}
	return -1
}

func fits(spans []span, start, width int) bool {
	if start+width > layerSlots {
		return false
	}
	for _, s := range spans {
		if start < s.end && s.start < start+width {
			return false
		}
	}
	return true
}

func (m *Module) adminPut(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id := r.PathValue("id")
	if !game.ValidID(id) {
		return httpx.NotFound("experiment not found")
	}
	var req putReq
	b, err := game.ReadBody(r, 64<<10)
	if err != nil {
		return err
	}
	if err := game.DecodeStrict(b, &req); err != nil {
		return err
	}
	if err := req.validate(); err != nil {
		return err
	}
	if req.Platforms == nil {
		req.Platforms = []string{}
	}
	g := game.CurrentGame(ctx)
	var out Experiment
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		now := m.now().UnixMilli()
		cur, err := getExperiment(ctx, tx, g.ID, id)
		exists := err == nil
		if err != nil {
			var he *httpx.Error
			if !errors.As(err, &he) || he.Status != http.StatusNotFound {
				return err
			}
		}
		e := Experiment{ID: id, Layer: req.Layer, Status: req.Status, Variants: req.Variants, Control: req.Control,
			TrafficPercent: *req.TrafficPercent, Platforms: req.Platforms, MinClientVersion: req.MinClientVersion}
		if exists {
			switch {
			case cur.Status == "stopped":
				return httpx.Conflict("experiment_stopped", "a stopped experiment cannot be changed; create a new experiment")
			case cur.Status == "running" && req.Status == "draft":
				return httpx.Conflict("invalid_transition", "a running experiment cannot return to draft")
			case cur.Status == "running" && (cur.Layer != e.Layer || cur.Control != e.Control || !sameVariants(cur.Variants, e.Variants)):
				return httpx.Conflict("immutable_while_running", "layer, variants and control cannot change while running; stop the experiment first")
			}
			e.SliceStart = cur.SliceStart
		}
		if e.Status == "running" {
			spans, err := runningSpans(ctx, tx, g.ID, e.Layer, id)
			if err != nil {
				return err
			}
			width := e.TrafficPercent * 100
			if exists && cur.Status == "running" {
				// Never relocate players of a running experiment: growing needs free room at the same start.
				if !fits(spans, e.SliceStart, width) {
					return httpx.Conflict("layer_full", "not enough free traffic in the layer at the current slice")
				}
			} else {
				start := firstFit(spans, width)
				if start < 0 {
					return httpx.Conflict("layer_full", "not enough free traffic in the layer")
				}
				e.SliceStart = start
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO exp_experiments(game_id,id,layer,status,variants,control,traffic_percent,platforms,min_client_version,slice_start,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(game_id,id) DO UPDATE SET layer=excluded.layer, status=excluded.status, variants=excluded.variants, control=excluded.control,
			  traffic_percent=excluded.traffic_percent, platforms=excluded.platforms, min_client_version=excluded.min_client_version,
			  slice_start=excluded.slice_start, updated_at=excluded.updated_at`,
			g.ID, id, e.Layer, e.Status, jsonStr(e.Variants), e.Control, e.TrafficPercent, jsonStr(e.Platforms), e.MinClientVersion, e.SliceStart, now, now)
		out = e
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, out.adminJSON())
	return nil
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// adminStop is the kill switch: everyone gets the control variant immediately.
// Stopping an already stopped experiment succeeds without change.
func (m *Module) adminStop(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id := r.PathValue("id")
	g := game.CurrentGame(ctx)
	var out Experiment
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		e, err := getExperiment(ctx, tx, g.ID, id)
		if err != nil {
			return err
		}
		if e.Status != "stopped" {
			if _, err := tx.ExecContext(ctx, `UPDATE exp_experiments SET status='stopped', updated_at=? WHERE game_id=? AND id=?`, m.now().UnixMilli(), g.ID, id); err != nil {
				return err
			}
			e.Status = "stopped"
		}
		out = e
		return nil
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, out.adminJSON())
	return nil
}

func (m *Module) adminList(w http.ResponseWriter, r *http.Request) error {
	es, err := listExperiments(r.Context(), m.DB.Read, game.CurrentGame(r.Context()).ID)
	if err != nil {
		return err
	}
	out := make([]map[string]any, len(es))
	for i, e := range es {
		out[i] = e.adminJSON()
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"experiments": out})
	return nil
}

func (m *Module) adminGet(w http.ResponseWriter, r *http.Request) error {
	e, err := getExperiment(r.Context(), m.DB.Read, game.CurrentGame(r.Context()).ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, e.adminJSON())
	return nil
}

// adminDelete removes a draft or stopped experiment together with its exposures.
func (m *Module) adminDelete(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id := r.PathValue("id")
	g := game.CurrentGame(ctx)
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		e, err := getExperiment(ctx, tx, g.ID, id)
		if err != nil {
			return err
		}
		if e.Status == "running" {
			return httpx.Conflict("experiment_running", "stop the experiment before deleting it")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM exp_exposures WHERE game_id=? AND experiment_id=?`, g.ID, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM exp_experiments WHERE game_id=? AND id=?`, g.ID, id)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// adminResults counts first exposures per variant.
func (m *Module) adminResults(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := game.CurrentGame(ctx)
	var e Experiment
	counts := map[string]int64{}
	err := m.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		if e, err = getExperiment(ctx, tx, g.ID, r.PathValue("id")); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT variant, count(*) FROM exp_exposures WHERE game_id=? AND experiment_id=? GROUP BY variant`, g.ID, e.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			var n int64
			if err := rows.Scan(&v, &n); err != nil {
				return err
			}
			counts[v] = n
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	var total int64
	vs := []map[string]any{}
	for _, v := range e.Variants {
		vs = append(vs, map[string]any{"variant": v.ID, "control": v.ID == e.Control, "exposures": counts[v.ID]})
		total += counts[v.ID]
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"experiment": e.ID, "status": e.Status, "variants": vs, "total_exposures": total})
	return nil
}
