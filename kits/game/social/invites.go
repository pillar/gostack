package social

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
)

var (
	codeRe    = regexp.MustCompile(`^[A-Z0-9]{4,16}$`)
	shareIDRe = regexp.MustCompile(`^shr_[a-z0-9]{26}$`)
)

func (m *Module) createInvite(w http.ResponseWriter, r *http.Request) error {
	if err := m.limit(r, "invite"); err != nil {
		return err
	}
	return m.mutate(w, r, "invites.create", nil, func(c *call) (idempotency.Result, error) {
		n, err := count(c.ctx, c.tx, `SELECT count(*) FROM invite_codes WHERE game_id=? AND inviter_id=?`, c.gid, c.pid)
		if err != nil {
			return idempotency.Result{}, err
		}
		if n >= m.MaxInvites {
			return idempotency.Result{}, httpx.Conflict("invite_limit", "invite code limit reached")
		}
		for i := 0; i < 10; i++ {
			code := randomCode(8)
			res, err := c.tx.ExecContext(c.ctx, `INSERT INTO invite_codes(game_id,code,inviter_id,created_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, c.gid, code, c.pid, c.now)
			if err != nil {
				return idempotency.Result{}, err
			}
			if k, _ := res.RowsAffected(); k == 1 {
				return idempotency.Result{Status: http.StatusCreated, Body: map[string]any{"code": code}}, nil
			}
		}
		return idempotency.Result{}, errors.New("invite code space exhausted")
	})
}

func (m *Module) listInvites(w http.ResponseWriter, r *http.Request) error {
	gid, pid := m.ident(r)
	rows, err := m.DB.Read.QueryContext(r.Context(), `SELECT c.code, c.created_at, (SELECT count(*) FROM invite_attributions a WHERE a.game_id=c.game_id AND a.code=c.code)
		FROM invite_codes c WHERE c.game_id=? AND c.inviter_id=? ORDER BY c.created_at, c.code`, gid, pid)
	if err != nil {
		return err
	}
	defer rows.Close()
	type inv struct {
		Code     string `json:"code"`
		At       int64  `json:"created_at"`
		Accepted int    `json:"accepted"`
	}
	out := []inv{}
	for rows.Next() {
		var i inv
		if err := rows.Scan(&i.Code, &i.At, &i.Accepted); err != nil {
			return err
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invites": out})
	return nil
}

// acceptInvite attributes the caller (a recently created player) to the
// inviter exactly once; the PRIMARY KEY on the invitee and the per-invitee ledger
// source keys make double attribution and double grants impossible.
func (m *Module) acceptInvite(w http.ResponseWriter, r *http.Request) error {
	if err := m.limit(r, "invite"); err != nil {
		return err
	}
	code := strings.ToUpper(r.PathValue("code"))
	if !codeRe.MatchString(code) {
		return httpx.NotFound("invite not found")
	}
	return m.mutate(w, r, "invites.accept:"+code, nil, func(c *call) (idempotency.Result, error) {
		var inviter string
		err := c.tx.QueryRowContext(c.ctx, `SELECT inviter_id FROM invite_codes WHERE game_id=? AND code=?`, c.gid, code).Scan(&inviter)
		if errors.Is(err, sql.ErrNoRows) {
			return idempotency.Result{}, httpx.NotFound("invite not found")
		}
		if err != nil {
			return idempotency.Result{}, err
		}
		if inviter == c.pid {
			return idempotency.Result{}, httpx.NewError(http.StatusBadRequest, "self_invite", "cannot accept your own invite")
		}
		if bl, err := blocked(c.ctx, c.tx, c.gid, c.pid, inviter); err != nil {
			return idempotency.Result{}, err
		} else if bl {
			return idempotency.Result{}, httpx.NotFound("invite not found")
		}
		var created int64
		if err := c.tx.QueryRowContext(c.ctx, `SELECT created_at FROM players WHERE game_id=? AND player_id=?`, c.gid, c.pid).Scan(&created); err != nil {
			return idempotency.Result{}, err
		}
		if created < c.now-m.InviteWindow.Milliseconds() {
			return idempotency.Result{}, httpx.NewError(http.StatusForbidden, "not_new_player", "invites can only be accepted by new players")
		}
		if n, err := count(c.ctx, c.tx, `SELECT count(*) FROM invite_attributions WHERE game_id=? AND invitee_id=?`, c.gid, c.pid); err != nil {
			return idempotency.Result{}, err
		} else if n > 0 {
			return idempotency.Result{}, httpx.Conflict("already_attributed", "an invite was already accepted")
		}
		if n, err := count(c.ctx, c.tx, `SELECT count(*) FROM invite_attributions WHERE game_id=? AND inviter_id=?`, c.gid, inviter); err != nil {
			return idempotency.Result{}, err
		} else if n >= m.MaxAcceptsPerInviter {
			return idempotency.Result{}, httpx.Conflict("invite_full", "this inviter reached the invite limit")
		}
		if _, err := c.tx.ExecContext(c.ctx, `INSERT INTO invite_attributions(game_id,invitee_id,code,inviter_id,created_at) VALUES(?,?,?,?,?)`, c.gid, c.pid, code, inviter, c.now); err != nil {
			return idempotency.Result{}, err
		}
		if len(m.InviterGrant) > 0 {
			if err := game.ApplyLedger(c.ctx, c.tx, c.gid, inviter, "invite", "invite:"+c.pid+":inviter", m.InviterGrant, c.now); err != nil {
				return idempotency.Result{}, err
			}
		}
		if len(m.InviteeGrant) > 0 {
			if err := game.ApplyLedger(c.ctx, c.tx, c.gid, c.pid, "invite", "invite:"+c.pid+":invitee", m.InviteeGrant, c.now); err != nil {
				return idempotency.Result{}, err
			}
		}
		return idempotency.Result{Body: map[string]any{"attributed": true, "grants": m.InviteeGrant}}, nil
	})
}

// ---- shares ----

func (m *Module) createShare(w http.ResponseWriter, r *http.Request) error {
	if err := m.limit(r, "share"); err != nil {
		return err
	}
	body, err := game.ReadBody(r, 4096)
	if err != nil {
		return err
	}
	var req struct {
		Channel string          `json:"channel"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	if !game.ValidID(req.Channel) {
		return httpx.BadRequest("channel must match [a-z0-9][a-z0-9_-]{0,31}")
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	}
	if len(req.Payload) > 2048 {
		return httpx.BadRequest("payload must be at most 2048 bytes")
	}
	return m.mutate(w, r, "shares.create", body, func(c *call) (idempotency.Result, error) {
		id := httpx.RandomID("shr_")
		if _, err := c.tx.ExecContext(c.ctx, `INSERT INTO shares(game_id,share_id,player_id,channel,payload,created_at) VALUES(?,?,?,?,?,?)`, c.gid, id, c.pid, req.Channel, string(req.Payload), c.now); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Status: http.StatusCreated, Body: map[string]any{"share_id": id}}, nil
	})
}

// getShare lets the sharer see how many distinct players opened a share.
func (m *Module) getShare(w http.ResponseWriter, r *http.Request) error {
	gid, pid := m.ident(r)
	id := r.PathValue("id")
	if !shareIDRe.MatchString(id) {
		return httpx.NotFound("share not found")
	}
	var channel, payload string
	var created int64
	var opens int
	err := m.DB.Read.QueryRowContext(r.Context(), `SELECT channel, payload, created_at, (SELECT count(*) FROM share_opens o WHERE o.game_id=s.game_id AND o.share_id=s.share_id)
		FROM shares s WHERE game_id=? AND share_id=? AND player_id=?`, gid, id, pid).Scan(&channel, &payload, &created, &opens)
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.NotFound("share not found")
	}
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"share_id": id, "channel": channel, "payload": json.RawMessage(payload), "created_at": created, "opens": opens})
	return nil
}

// openShare records one open per (share, player); no reward is granted.
func (m *Module) openShare(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if !shareIDRe.MatchString(id) {
		return httpx.NotFound("share not found")
	}
	return m.mutate(w, r, "shares.open:"+id, nil, func(c *call) (idempotency.Result, error) {
		var owner, channel, payload string
		err := c.tx.QueryRowContext(c.ctx, `SELECT player_id, channel, payload FROM shares WHERE game_id=? AND share_id=?`, c.gid, id).Scan(&owner, &channel, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			return idempotency.Result{}, httpx.NotFound("share not found")
		}
		if err != nil {
			return idempotency.Result{}, err
		}
		if owner == c.pid {
			return idempotency.Result{}, httpx.NewError(http.StatusBadRequest, "own_share", "cannot open your own share")
		}
		if bl, err := blocked(c.ctx, c.tx, c.gid, c.pid, owner); err != nil {
			return idempotency.Result{}, err
		} else if bl {
			return idempotency.Result{}, httpx.NotFound("share not found")
		}
		res, err := c.tx.ExecContext(c.ctx, `INSERT INTO share_opens(game_id,share_id,opener_id,created_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, c.gid, id, c.pid, c.now)
		if err != nil {
			return idempotency.Result{}, err
		}
		n, _ := res.RowsAffected()
		return idempotency.Result{Body: map[string]any{"share_id": id, "channel": channel, "payload": json.RawMessage(payload), "first_open": n == 1}}, nil
	})
}
