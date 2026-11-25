package game

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/idempotency"
)

// SaveMergeFunc merges a conflicting save. base is the payload of the revision
// the client edited (nil when the client created the slot from scratch), server
// the current payload and client the rejected one. It runs inside the write
// transaction: it must be fast, deterministic, re-runnable and do no network IO.
type SaveMergeFunc func(ctx context.Context, slot string, base, server, client json.RawMessage) (json.RawMessage, error)

type saveDTO struct {
	Slot          string          `json:"slot"`
	Revision      int64           `json:"revision"`
	SchemaVersion int64           `json:"schema_version"`
	Checksum      string          `json:"checksum"`
	Payload       json.RawMessage `json:"payload"`
	UpdatedAt     int64           `json:"updated_at"`
	Merged        bool            `json:"merged,omitempty"`
}

// saveJSON renders d with the payload bytes exactly as stored (json.Marshal
// would compact and escape RawMessage, breaking the checksum contract).
func saveJSON(d saveDTO, payload []byte) []byte {
	merged := ""
	if d.Merged {
		merged = `,"merged":true`
	}
	return fmt.Appendf(nil, `{"slot":%q,"revision":%d,"schema_version":%d,"checksum":%q,"updated_at":%d%s,"payload":%s}`,
		d.Slot, d.Revision, d.SchemaVersion, d.Checksum, d.UpdatedAt, merged, payload)
}

func writeSave(w http.ResponseWriter, d saveDTO, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(saveJSON(d, payload), '\n'))
}

func (k *Kit) slot(r *http.Request) (string, error) {
	s := r.PathValue("slot")
	if !slotPattern.MatchString(s) {
		return "", httpx.BadRequest("slot must match [a-z0-9_-]{1,32}")
	}
	return s, nil
}

func checksumOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (k *Kit) getSave(w http.ResponseWriter, r *http.Request) error {
	s := sessionFrom(r.Context())
	slot, err := k.slot(r)
	if err != nil {
		return err
	}
	d := saveDTO{Slot: slot}
	var payload []byte
	err = k.DB.Read.QueryRowContext(r.Context(), `SELECT revision, schema_version, checksum, payload, updated_at FROM save_slots
		WHERE game_id=? AND player_id=? AND slot=?`, s.GameID, s.PlayerID, slot).Scan(&d.Revision, &d.SchemaVersion, &d.Checksum, &payload, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.NotFound("save not found")
	}
	if err != nil {
		return err
	}
	writeSave(w, d, payload)
	return nil
}

func currentRevision(ctx context.Context, tx *sql.Tx, s *session, slot string) (int64, error) {
	var current int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM save_slots WHERE game_id=? AND player_id=? AND slot=?`, s.GameID, s.PlayerID, slot).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return current, nil
}

func revisionConflict(current int64) error {
	e := httpx.Conflict("revision_conflict", "save revision mismatch")
	e.Details = map[string]any{"current_revision": current}
	return e
}

func (k *Kit) checkSchema(ctx context.Context, q queryer, gameID string, v int64) error {
	c, err := k.currentConfig(ctx, q, gameID)
	if err != nil {
		return err
	}
	if c.Doc.Saves != nil && v < c.Doc.Saves.MinSchemaVersion {
		e := httpx.Conflict("schema_outdated", "save schema_version is below the minimum supported version")
		e.Details = map[string]any{"min_schema_version": c.Doc.Saves.MinSchemaVersion}
		return e
	}
	return nil
}

// commitRevision stores d as the slot's next revision (current+1) and appends it
// to the bounded history.
func (k *Kit) commitRevision(ctx context.Context, tx *sql.Tx, s *session, d saveDTO) error {
	if d.Revision == 1 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM save_slots WHERE game_id=? AND player_id=?`, s.GameID, s.PlayerID).Scan(&n); err != nil {
			return err
		}
		if n >= k.Cfg.SaveSlotsPerUser {
			return httpx.Conflict("slot_limit", "save slot limit reached")
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO save_slots(game_id,player_id,slot,revision,schema_version,checksum,payload,updated_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(game_id,player_id,slot) DO UPDATE SET
		revision=excluded.revision, schema_version=excluded.schema_version, checksum=excluded.checksum, payload=excluded.payload, updated_at=excluded.updated_at`,
		s.GameID, s.PlayerID, d.Slot, d.Revision, d.SchemaVersion, d.Checksum, []byte(d.Payload), d.UpdatedAt); err != nil {
		return err
	}
	if k.Cfg.SaveHistory > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO save_revisions(game_id,player_id,slot,revision,schema_version,checksum,payload,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			s.GameID, s.PlayerID, d.Slot, d.Revision, d.SchemaVersion, d.Checksum, []byte(d.Payload), d.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM save_revisions WHERE game_id=? AND player_id=? AND slot=? AND revision<=?`,
			s.GameID, s.PlayerID, d.Slot, d.Revision-int64(k.Cfg.SaveHistory)); err != nil {
			return err
		}
	}
	return nil
}

// putSave performs a conditional write keyed on expected_revision (G03).
// The server never decides by client clocks; a stale revision gets 409 unless
// the client asks for a merge and the application configured SaveMerge.
func (k *Kit) putSave(w http.ResponseWriter, r *http.Request) error {
	s := sessionFrom(r.Context())
	slot, err := k.slot(r)
	if err != nil {
		return err
	}
	body, err := readBody(r, int64(k.Cfg.SaveMaxBytes)+4096)
	if err != nil {
		return err
	}
	var req struct {
		ExpectedRevision *int64          `json:"expected_revision"`
		SchemaVersion    int64           `json:"schema_version"`
		Payload          json.RawMessage `json:"payload"`
		Checksum         string          `json:"checksum"`
		Merge            bool            `json:"merge"`
	}
	if err := decodeStrict(body, &req); err != nil {
		return err
	}
	if req.ExpectedRevision == nil || *req.ExpectedRevision < 0 {
		return httpx.BadRequest("expected_revision (>= 0) is required")
	}
	if len(req.Payload) == 0 || bytes.Equal(req.Payload, []byte("null")) {
		return httpx.BadRequest("payload required")
	}
	if len(req.Payload) > k.Cfg.SaveMaxBytes {
		return httpx.TooLarge("save payload exceeds limit")
	}
	if req.SchemaVersion < 0 {
		return httpx.BadRequest("schema_version must be >= 0")
	}
	checksum := checksumOf(req.Payload)
	if req.Checksum != "" && req.Checksum != checksum {
		return httpx.BadRequest("checksum mismatch")
	}
	ctx := r.Context()
	status, resp, replayed, err := idempotency.Do(ctx, k.DB, "game:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), "save.put:"+slot, body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			if err := k.checkSchema(ctx, tx, s.GameID, req.SchemaVersion); err != nil {
				return idempotency.Result{}, err
			}
			current, err := currentRevision(ctx, tx, s, slot)
			if err != nil {
				return idempotency.Result{}, err
			}
			d := saveDTO{Slot: slot, Revision: current + 1, SchemaVersion: req.SchemaVersion, Checksum: checksum, Payload: req.Payload, UpdatedAt: k.Now().UnixMilli()}
			if current != *req.ExpectedRevision {
				if !req.Merge || k.SaveMerge == nil || current == 0 {
					return idempotency.Result{}, revisionConflict(current)
				}
				merged, err := k.mergeSave(ctx, tx, s, slot, *req.ExpectedRevision, req.Payload)
				if err != nil {
					return idempotency.Result{}, err
				}
				d.Payload, d.Checksum, d.Merged = merged, checksumOf(merged), true
			}
			if err := k.commitRevision(ctx, tx, s, d); err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Raw: saveJSON(d, d.Payload)}, nil
		})
	if err != nil {
		return mapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

func (k *Kit) mergeSave(ctx context.Context, tx *sql.Tx, s *session, slot string, baseRev int64, client json.RawMessage) (json.RawMessage, error) {
	var server, base []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM save_slots WHERE game_id=? AND player_id=? AND slot=?`, s.GameID, s.PlayerID, slot).Scan(&server); err != nil {
		return nil, err
	}
	if baseRev > 0 {
		err := tx.QueryRowContext(ctx, `SELECT payload FROM save_revisions WHERE game_id=? AND player_id=? AND slot=? AND revision=?`,
			s.GameID, s.PlayerID, slot, baseRev).Scan(&base)
		if errors.Is(err, sql.ErrNoRows) {
			e := httpx.Conflict("merge_base_unavailable", "base revision is no longer retained; reload and retry")
			e.Details = map[string]any{"expected_revision": baseRev}
			return nil, e
		}
		if err != nil {
			return nil, err
		}
	}
	merged, err := k.SaveMerge(ctx, slot, base, server, client)
	if err != nil {
		return nil, err
	}
	if len(merged) == 0 || !json.Valid(merged) || bytes.Equal(merged, []byte("null")) {
		return nil, errors.New("save merge returned invalid JSON")
	}
	if len(merged) > k.Cfg.SaveMaxBytes {
		return nil, httpx.TooLarge("merged save payload exceeds limit")
	}
	return merged, nil
}

// listSaveRevisions returns retained revision metadata, newest first.
func (k *Kit) listSaveRevisions(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := sessionFrom(ctx)
	slot, err := k.slot(r)
	if err != nil {
		return err
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100 {
			return httpx.BadRequest("limit must be within [1,100]")
		}
	}
	type rev struct {
		Revision      int64  `json:"revision"`
		SchemaVersion int64  `json:"schema_version"`
		Checksum      string `json:"checksum"`
		Size          int64  `json:"size"`
		CreatedAt     int64  `json:"created_at"`
	}
	out := struct {
		Slot            string `json:"slot"`
		CurrentRevision int64  `json:"current_revision"`
		Revisions       []rev  `json:"revisions"`
	}{Slot: slot, Revisions: []rev{}}
	err = k.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(revision),0) FROM save_slots WHERE game_id=? AND player_id=? AND slot=?`,
			s.GameID, s.PlayerID, slot).Scan(&out.CurrentRevision); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT revision, schema_version, checksum, length(payload), created_at FROM save_revisions
			WHERE game_id=? AND player_id=? AND slot=? ORDER BY revision DESC LIMIT ?`, s.GameID, s.PlayerID, slot, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v rev
			if err := rows.Scan(&v.Revision, &v.SchemaVersion, &v.Checksum, &v.Size, &v.CreatedAt); err != nil {
				return err
			}
			out.Revisions = append(out.Revisions, v)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if out.CurrentRevision == 0 && len(out.Revisions) == 0 {
		return httpx.NotFound("save not found")
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// restoreSave writes a retained revision's payload as the next revision.
func (k *Kit) restoreSave(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := sessionFrom(ctx)
	slot, err := k.slot(r)
	if err != nil {
		return err
	}
	body, err := readBody(r, 1024)
	if err != nil {
		return err
	}
	var req struct {
		Revision         int64  `json:"revision"`
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	if err := decodeStrict(body, &req); err != nil {
		return err
	}
	if req.Revision < 1 || req.ExpectedRevision == nil || *req.ExpectedRevision < 1 {
		return httpx.BadRequest("revision (>= 1) and expected_revision (>= 1) are required")
	}
	status, resp, replayed, err := idempotency.Do(ctx, k.DB, "game:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), "save.restore:"+slot, body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			current, err := currentRevision(ctx, tx, s, slot)
			if err != nil {
				return idempotency.Result{}, err
			}
			if current != *req.ExpectedRevision {
				return idempotency.Result{}, revisionConflict(current)
			}
			d := saveDTO{Slot: slot, Revision: current + 1, UpdatedAt: k.Now().UnixMilli()}
			var payload []byte
			err = tx.QueryRowContext(ctx, `SELECT schema_version, checksum, payload FROM save_revisions WHERE game_id=? AND player_id=? AND slot=? AND revision=?`,
				s.GameID, s.PlayerID, slot, req.Revision).Scan(&d.SchemaVersion, &d.Checksum, &payload)
			if errors.Is(err, sql.ErrNoRows) {
				return idempotency.Result{}, httpx.NotFound("revision not retained")
			}
			if err != nil {
				return idempotency.Result{}, err
			}
			if err := k.checkSchema(ctx, tx, s.GameID, d.SchemaVersion); err != nil {
				return idempotency.Result{}, err
			}
			d.Payload = payload
			if err := k.commitRevision(ctx, tx, s, d); err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Raw: saveJSON(d, d.Payload)}, nil
		})
	if err != nil {
		return mapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

// deleteSave removes a slot and its retained history, conditional on
// expected_revision (query parameter). The revision counter restarts at 1.
func (k *Kit) deleteSave(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := sessionFrom(ctx)
	slot, err := k.slot(r)
	if err != nil {
		return err
	}
	expected, err := strconv.ParseInt(r.URL.Query().Get("expected_revision"), 10, 64)
	if err != nil || expected < 1 {
		return httpx.BadRequest("expected_revision (>= 1) query parameter is required")
	}
	status, resp, replayed, err := idempotency.Do(ctx, k.DB, "game:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), "save.delete:"+slot, []byte(strconv.FormatInt(expected, 10)),
		func(tx *sql.Tx) (idempotency.Result, error) {
			current, err := currentRevision(ctx, tx, s, slot)
			if err != nil {
				return idempotency.Result{}, err
			}
			if current == 0 {
				return idempotency.Result{}, httpx.NotFound("save not found")
			}
			if current != expected {
				return idempotency.Result{}, revisionConflict(current)
			}
			for _, q := range []string{`DELETE FROM save_slots WHERE game_id=? AND player_id=? AND slot=?`, `DELETE FROM save_revisions WHERE game_id=? AND player_id=? AND slot=?`} {
				if _, err := tx.ExecContext(ctx, q, s.GameID, s.PlayerID, slot); err != nil {
					return idempotency.Result{}, err
				}
			}
			return idempotency.Result{Body: map[string]any{"slot": slot, "deleted": true, "revision": current}}, nil
		})
	if err != nil {
		return mapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}
