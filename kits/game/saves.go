package game

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/idempotency"
)

type saveDTO struct {
	Slot          string          `json:"slot"`
	Revision      int64           `json:"revision"`
	SchemaVersion int64           `json:"schema_version"`
	Checksum      string          `json:"checksum"`
	Payload       json.RawMessage `json:"payload"`
	UpdatedAt     int64           `json:"updated_at"`
}

// writeSave emits the payload bytes exactly as stored (json.Marshal would compact
// and escape RawMessage, breaking the checksum contract).
func writeSave(w http.ResponseWriter, d saveDTO, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"slot":%q,"revision":%d,"schema_version":%d,"checksum":%q,"updated_at":%d,"payload":%s}`+"\n",
		d.Slot, d.Revision, d.SchemaVersion, d.Checksum, d.UpdatedAt, payload)
}

func (k *Kit) slot(r *http.Request) (string, error) {
	s := r.PathValue("slot")
	if !slotPattern.MatchString(s) {
		return "", httpx.BadRequest("slot must match [a-z0-9_-]{1,32}")
	}
	return s, nil
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

// putSave performs a conditional write keyed on expected_revision (G03).
// The server never decides by client clocks; a stale revision gets 409.
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
	sum := sha256.Sum256(req.Payload)
	checksum := hex.EncodeToString(sum[:])
	if req.Checksum != "" && req.Checksum != checksum {
		return httpx.BadRequest("checksum mismatch")
	}
	ctx := r.Context()
	status, resp, replayed, err := idempotency.Do(ctx, k.DB, "game:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), "save.put:"+slot, body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			var current int64
			err := tx.QueryRowContext(ctx, `SELECT revision FROM save_slots WHERE game_id=? AND player_id=? AND slot=?`, s.GameID, s.PlayerID, slot).Scan(&current)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return idempotency.Result{}, err
			}
			if current != *req.ExpectedRevision {
				e := httpx.Conflict("revision_conflict", "save revision mismatch")
				e.Details = map[string]any{"current_revision": current}
				return idempotency.Result{}, e
			}
			if current == 0 {
				var n int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM save_slots WHERE game_id=? AND player_id=?`, s.GameID, s.PlayerID).Scan(&n); err != nil {
					return idempotency.Result{}, err
				}
				if n >= k.Cfg.SaveSlotsPerUser {
					return idempotency.Result{}, httpx.Conflict("slot_limit", "save slot limit reached")
				}
			}
			now := k.Now().UnixMilli()
			d := saveDTO{Slot: slot, Revision: current + 1, SchemaVersion: req.SchemaVersion, Checksum: checksum, Payload: req.Payload, UpdatedAt: now}
			if _, err := tx.ExecContext(ctx, `INSERT INTO save_slots(game_id,player_id,slot,revision,schema_version,checksum,payload,updated_at)
				VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(game_id,player_id,slot) DO UPDATE SET
				revision=excluded.revision, schema_version=excluded.schema_version, checksum=excluded.checksum, payload=excluded.payload, updated_at=excluded.updated_at`,
				s.GameID, s.PlayerID, slot, d.Revision, d.SchemaVersion, checksum, []byte(req.Payload), now); err != nil {
				return idempotency.Result{}, err
			}
			if k.Cfg.SaveHistory > 0 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO save_revisions(game_id,player_id,slot,revision,schema_version,checksum,payload,created_at) VALUES(?,?,?,?,?,?,?,?)`,
					s.GameID, s.PlayerID, slot, d.Revision, d.SchemaVersion, checksum, []byte(req.Payload), now); err != nil {
					return idempotency.Result{}, err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM save_revisions WHERE game_id=? AND player_id=? AND slot=? AND revision<=?`,
					s.GameID, s.PlayerID, slot, d.Revision-int64(k.Cfg.SaveHistory)); err != nil {
					return idempotency.Result{}, err
				}
			}
			return idempotency.Result{Body: d}, nil
		})
	if err != nil {
		return mapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}
