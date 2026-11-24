package ops

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
)

// idem runs fn under the request's Idempotency-Key (if any) and writes the result.
func (m *Module) idem(w http.ResponseWriter, r *http.Request, scope, op string, body []byte, fn func(tx *sql.Tx) (idempotency.Result, error)) error {
	status, resp, replayed, err := idempotency.Do(r.Context(), m.DB, scope, r.Header.Get("Idempotency-Key"), op, body, fn)
	if err != nil {
		return game.MapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

type mailReq struct {
	PlayerID  string  `json:"player_id"`
	Broadcast bool    `json:"broadcast"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	Grants    Grants  `json:"grants"`
	ExpiresAt Instant `json:"expires_at"`
}

// adminSendMail stores one mail row: addressed to one player, or a broadcast that
// players see lazily (no per-player fan-out).
func (m *Module) adminSendMail(w http.ResponseWriter, r *http.Request) error {
	body, err := game.ReadBody(r, 32<<10)
	if err != nil {
		return err
	}
	var req mailReq
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	if req.Broadcast == (req.PlayerID != "") {
		return httpx.BadRequest("set exactly one of player_id or broadcast=true")
	}
	if req.PlayerID != "" && len(req.PlayerID) > 64 {
		return httpx.BadRequest("invalid player_id")
	}
	if req.Title == "" || len(req.Title) > 200 || len(req.Body) > 8000 {
		return httpx.BadRequest("title is required (<=200 bytes) and body must be <=8000 bytes")
	}
	if err := validGrants(req.Grants, false); err != nil {
		return err
	}
	now := m.now().UnixMilli()
	if req.ExpiresAt != 0 && int64(req.ExpiresAt) <= now {
		return httpx.BadRequest("expires_at must be in the future")
	}
	g := game.CurrentGame(r.Context())
	return m.idem(w, r, "ops-admin:"+g.ID, "mail.send", body, func(tx *sql.Tx) (idempotency.Result, error) {
		var target any
		if req.PlayerID != "" {
			if err := game.EnsurePlayer(r.Context(), tx, g.ID, req.PlayerID); err != nil {
				return idempotency.Result{}, err
			}
			target = req.PlayerID
		}
		id := httpx.RandomID("mail_")
		_, err := tx.ExecContext(r.Context(), `INSERT INTO ops_mail(game_id,mail_id,target_player,title,body,grants,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
			g.ID, id, target, req.Title, req.Body, jsonString(req.Grants), now, int64(req.ExpiresAt))
		return idempotency.Result{Status: http.StatusCreated, Body: map[string]any{"mail_id": id, "broadcast": req.Broadcast}}, err
	})
}

func (m *Module) adminListMail(w http.ResponseWriter, r *http.Request) error {
	g := game.CurrentGame(r.Context())
	rows, err := m.DB.Read.QueryContext(r.Context(), `SELECT m.mail_id, coalesce(m.target_player,''), m.title, m.grants, m.created_at, m.expires_at,
		(SELECT count(*) FROM ops_mail_state s WHERE s.game_id=m.game_id AND s.mail_id=m.mail_id AND s.claimed_at>0)
		FROM ops_mail m WHERE m.game_id=? ORDER BY m.created_at DESC, m.mail_id DESC LIMIT 100`, g.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, target, title, grants string
		var created, expires, claimed int64
		if err := rows.Scan(&id, &target, &title, &grants, &created, &expires, &claimed); err != nil {
			return err
		}
		out = append(out, map[string]any{"mail_id": id, "player_id": target, "broadcast": target == "", "title": title,
			"grants": parseGrants(grants), "created_at": created, "expires_at": expires, "claimed_count": claimed})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"mail": out})
	return nil
}

// adminDeleteMail recalls a mail for everyone (claims already paid stay in the ledger).
func (m *Module) adminDeleteMail(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	g := game.CurrentGame(r.Context())
	var n int64
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM ops_mail_state WHERE game_id=? AND mail_id=?`, g.ID, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(r.Context(), `DELETE FROM ops_mail WHERE game_id=? AND mail_id=?`, g.ID, id)
		if err == nil {
			n, err = res.RowsAffected()
		}
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	if n == 0 {
		return httpx.NotFound("mail not found")
	}
	return writeNoContent(w)
}

// ---- player side ----

type mailOut struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Grants    Grants `json:"grants"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	Read      bool   `json:"read"`
	Claimed   bool   `json:"claimed"`
	Claimable bool   `json:"claimable"`
}

// A mail is visible to a player when it is addressed to them, or is a broadcast
// sent at or after the player's registration; and is neither expired nor hidden.
const visibleMailSQL = `SELECT m.mail_id, m.title, m.body, m.grants, m.created_at, m.expires_at, coalesce(s.read_at,0), coalesce(s.claimed_at,0)
	FROM ops_mail m
	JOIN players p ON p.game_id=m.game_id AND p.player_id=?2
	LEFT JOIN ops_mail_state s ON s.game_id=m.game_id AND s.mail_id=m.mail_id AND s.player_id=p.player_id
	WHERE m.game_id=?1 AND (m.target_player=?2 OR (m.target_player IS NULL AND m.created_at>=p.created_at))
	  AND (m.expires_at=0 OR m.expires_at>?3) AND coalesce(s.deleted_at,0)=0`

func scanMail(sc interface{ Scan(...any) error }) (mailOut, error) {
	var o mailOut
	var grants string
	var read, claimed int64
	if err := sc.Scan(&o.ID, &o.Title, &o.Body, &grants, &o.CreatedAt, &o.ExpiresAt, &read, &claimed); err != nil {
		return o, err
	}
	o.Grants = parseGrants(grants)
	o.Read, o.Claimed = read > 0, claimed > 0
	o.Claimable = len(o.Grants) > 0 && !o.Claimed
	return o, nil
}

func visibleMail(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gameID, playerID, mailID string, nowMs int64) (mailOut, error) {
	o, err := scanMail(q.QueryRowContext(ctx, visibleMailSQL+` AND m.mail_id=?4`, gameID, playerID, nowMs, mailID))
	if errors.Is(err, sql.ErrNoRows) {
		return o, httpx.NotFound("mail not found")
	}
	return o, err
}

func (m *Module) listMail(w http.ResponseWriter, r *http.Request) error {
	s := game.CurrentSession(r.Context())
	rows, err := m.DB.Read.QueryContext(r.Context(), visibleMailSQL+` ORDER BY m.created_at DESC, m.mail_id DESC LIMIT 100`, s.GameID, s.PlayerID, m.now().UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []mailOut{}
	for rows.Next() {
		o, err := scanMail(rows)
		if err != nil {
			return err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"mail": out})
	return nil
}

func ensureState(ctx context.Context, tx *sql.Tx, gameID, mailID, playerID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ops_mail_state(game_id,mail_id,player_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`, gameID, mailID, playerID)
	return err
}

func (m *Module) readMail(w http.ResponseWriter, r *http.Request) error {
	s := game.CurrentSession(r.Context())
	id := r.PathValue("id")
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		now := m.now().UnixMilli()
		if _, err := visibleMail(r.Context(), tx, s.GameID, s.PlayerID, id, now); err != nil {
			return err
		}
		if err := ensureState(r.Context(), tx, s.GameID, id, s.PlayerID); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `UPDATE ops_mail_state SET read_at=? WHERE game_id=? AND mail_id=? AND player_id=? AND read_at=0`, now, s.GameID, id, s.PlayerID)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	return writeNoContent(w)
}

// claimMail pays the attachment once per (mail, player): the state row and the
// ledger source key "mail:<id>:<player>" both guard against a second payment.
func (m *Module) claimMail(w http.ResponseWriter, r *http.Request) error {
	s := game.CurrentSession(r.Context())
	id := r.PathValue("id")
	return m.idem(w, r, "game:"+s.GameID+":"+s.PlayerID, "mail.claim:"+id, nil, func(tx *sql.Tx) (idempotency.Result, error) {
		ctx := r.Context()
		now := m.now().UnixMilli()
		mail, err := visibleMail(ctx, tx, s.GameID, s.PlayerID, id, now)
		if err != nil {
			return idempotency.Result{}, err
		}
		if len(mail.Grants) == 0 {
			return idempotency.Result{}, httpx.Conflict("no_attachment", "mail has no attachment")
		}
		if mail.Claimed {
			return idempotency.Result{}, httpx.Conflict("already_claimed", "attachment already claimed")
		}
		if err := ensureState(ctx, tx, s.GameID, id, s.PlayerID); err != nil {
			return idempotency.Result{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ops_mail_state SET claimed_at=?, read_at=CASE WHEN read_at=0 THEN ? ELSE read_at END
			WHERE game_id=? AND mail_id=? AND player_id=?`, now, now, s.GameID, id, s.PlayerID); err != nil {
			return idempotency.Result{}, err
		}
		if err := game.ApplyLedger(ctx, tx, s.GameID, s.PlayerID, "reward", "mail:"+id+":"+s.PlayerID, mail.Grants, now); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Body: map[string]any{"mail_id": id, "grants": mail.Grants}}, nil
	})
}

// deleteMail hides a mail for this player. A mail with an unclaimed attachment is
// refused (409 unclaimed_attachments) unless ?discard=true, which forfeits it.
func (m *Module) deleteMail(w http.ResponseWriter, r *http.Request) error {
	s := game.CurrentSession(r.Context())
	id := r.PathValue("id")
	discard := r.URL.Query().Get("discard") == "true"
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		now := m.now().UnixMilli()
		mail, err := visibleMail(r.Context(), tx, s.GameID, s.PlayerID, id, now)
		if err != nil {
			return err
		}
		if mail.Claimable && !discard {
			return httpx.Conflict("unclaimed_attachments", "claim the attachment first or pass discard=true to forfeit it")
		}
		if err := ensureState(r.Context(), tx, s.GameID, id, s.PlayerID); err != nil {
			return err
		}
		_, err = tx.ExecContext(r.Context(), `UPDATE ops_mail_state SET deleted_at=? WHERE game_id=? AND mail_id=? AND player_id=?`, now, s.GameID, id, s.PlayerID)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	return writeNoContent(w)
}
