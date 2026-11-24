package social

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
)

var (
	errRequestsDisabled = httpx.NewError(http.StatusForbidden, "requests_disabled", "this player does not accept friend requests")
	errFriendLimit      = httpx.Conflict("friend_limit", "friend limit reached")
)

type person struct {
	PlayerID    string `json:"player_id"`
	DisplayName string `json:"display_name"`
	At          int64  `json:"at"`
}

func (m *Module) queryPeople(r *http.Request, query string, args ...any) ([]person, error) {
	rows, err := m.DB.Read.QueryContext(r.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []person{}
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.PlayerID, &p.DisplayName, &p.At); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (m *Module) listFriends(w http.ResponseWriter, r *http.Request) error {
	gid, pid := m.ident(r)
	ps, err := m.queryPeople(r, `SELECT f.friend_id, coalesce(p.display_name,''), f.created_at FROM friendships f
		LEFT JOIN social_profiles p ON p.game_id=f.game_id AND p.player_id=f.friend_id
		WHERE f.game_id=? AND f.player_id=? ORDER BY f.created_at, f.friend_id LIMIT ?`, gid, pid, m.MaxFriends)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"friends": ps})
	return nil
}

func (m *Module) listRequests(w http.ResponseWriter, r *http.Request) error {
	gid, pid := m.ident(r)
	in, err := m.queryPeople(r, `SELECT q.from_player, coalesce(p.display_name,''), q.created_at FROM friend_requests q
		LEFT JOIN social_profiles p ON p.game_id=q.game_id AND p.player_id=q.from_player
		WHERE q.game_id=? AND q.to_player=? ORDER BY q.created_at, q.from_player LIMIT 200`, gid, pid)
	if err != nil {
		return err
	}
	out, err := m.queryPeople(r, `SELECT q.to_player, coalesce(p.display_name,''), q.created_at FROM friend_requests q
		LEFT JOIN social_profiles p ON p.game_id=q.game_id AND p.player_id=q.to_player
		WHERE q.game_id=? AND q.from_player=? ORDER BY q.created_at, q.to_player LIMIT 200`, gid, pid)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"incoming": in, "outgoing": out})
	return nil
}

// makeFriends stores the symmetric friendship and clears pending requests in
// both directions inside the caller's transaction.
func (m *Module) makeFriends(c *call, a, b string) error {
	for _, p := range []string{a, b} {
		n, err := count(c.ctx, c.tx, `SELECT count(*) FROM friendships WHERE game_id=? AND player_id=?`, c.gid, p)
		if err != nil {
			return err
		}
		if n >= m.MaxFriends {
			return errFriendLimit
		}
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		if _, err := c.tx.ExecContext(c.ctx, `INSERT OR IGNORE INTO friendships(game_id,player_id,friend_id,created_at) VALUES(?,?,?,?)`, c.gid, pair[0], pair[1], c.now); err != nil {
			return err
		}
	}
	_, err := c.tx.ExecContext(c.ctx, `DELETE FROM friend_requests WHERE game_id=? AND ((from_player=? AND to_player=?) OR (from_player=? AND to_player=?))`, c.gid, a, b, b, a)
	return err
}

func (m *Module) sendRequest(w http.ResponseWriter, r *http.Request) error {
	if err := m.limit(r, "friendreq"); err != nil {
		return err
	}
	body, err := game.ReadBody(r, 1024)
	if err != nil {
		return err
	}
	var req struct {
		PlayerID   string `json:"player_id"`
		FriendCode string `json:"friend_code"`
	}
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	if (req.PlayerID == "") == (req.FriendCode == "") {
		return httpx.BadRequest("exactly one of player_id or friend_code is required")
	}
	if req.PlayerID != "" && !idRe.MatchString(req.PlayerID) {
		return errNotFound
	}
	return m.mutate(w, r, "friends.request", body, func(c *call) (idempotency.Result, error) {
		target := req.PlayerID
		if req.FriendCode != "" {
			err := c.tx.QueryRowContext(c.ctx, `SELECT player_id FROM social_profiles WHERE game_id=? AND friend_code=?`, c.gid, strings.ToUpper(req.FriendCode)).Scan(&target)
			if errors.Is(err, sql.ErrNoRows) {
				return idempotency.Result{}, errNotFound
			}
			if err != nil {
				return idempotency.Result{}, err
			}
		}
		if target == c.pid {
			return idempotency.Result{}, httpx.BadRequest("cannot send a friend request to yourself")
		}
		if err := game.EnsurePlayer(c.ctx, c.tx, c.gid, target); err != nil {
			return idempotency.Result{}, errNotFound
		}
		if bl, err := blocked(c.ctx, c.tx, c.gid, c.pid, target); err != nil || bl {
			if err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{}, errNotFound
		}
		incoming, err := count(c.ctx, c.tx, `SELECT count(*) FROM friend_requests WHERE game_id=? AND from_player=? AND to_player=?`, c.gid, target, c.pid)
		if err != nil {
			return idempotency.Result{}, err
		}
		// Players default to searchable/everyone until they create a profile.
		tp := privacy{Searchable: true, FriendRequests: "everyone"}
		if p, err := scanProfile(c.tx.QueryRowContext(c.ctx, profileCols, c.gid, target), target); err == nil {
			tp = p.Privacy
		} else if !errors.Is(err, sql.ErrNoRows) {
			return idempotency.Result{}, err
		}
		if !tp.Searchable && incoming == 0 {
			return idempotency.Result{}, errNotFound // hidden: same as unknown
		}
		if n, err := count(c.ctx, c.tx, `SELECT count(*) FROM friendships WHERE game_id=? AND player_id=? AND friend_id=?`, c.gid, c.pid, target); err != nil {
			return idempotency.Result{}, err
		} else if n > 0 {
			return idempotency.Result{}, httpx.Conflict("already_friends", "already friends")
		}
		if incoming > 0 { // they already asked us: accept
			if err := m.makeFriends(c, c.pid, target); err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Body: map[string]any{"status": "accepted", "player_id": target}}, nil
		}
		if tp.FriendRequests == "nobody" {
			return idempotency.Result{}, errRequestsDisabled
		}
		if n, err := count(c.ctx, c.tx, `SELECT count(*) FROM friend_requests WHERE game_id=? AND from_player=? AND to_player=?`, c.gid, c.pid, target); err != nil {
			return idempotency.Result{}, err
		} else if n > 0 {
			return idempotency.Result{}, httpx.Conflict("already_requested", "a request is already pending")
		}
		if n, err := count(c.ctx, c.tx, `SELECT count(*) FROM friend_requests WHERE game_id=? AND from_player=?`, c.gid, c.pid); err != nil {
			return idempotency.Result{}, err
		} else if n >= m.MaxPendingRequests {
			return idempotency.Result{}, httpx.Conflict("request_limit", "too many pending requests")
		}
		if n, err := count(c.ctx, c.tx, `SELECT count(*) FROM friendships WHERE game_id=? AND player_id=?`, c.gid, c.pid); err != nil {
			return idempotency.Result{}, err
		} else if n >= m.MaxFriends {
			return idempotency.Result{}, errFriendLimit
		}
		if _, err := c.tx.ExecContext(c.ctx, `INSERT INTO friend_requests(game_id,from_player,to_player,created_at) VALUES(?,?,?,?)`, c.gid, c.pid, target, c.now); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Status: http.StatusCreated, Body: map[string]any{"status": "pending", "player_id": target}}, nil
	})
}

func (m *Module) acceptRequest(w http.ResponseWriter, r *http.Request) error {
	from, err := pathPlayer(r)
	if err != nil {
		return err
	}
	return m.mutate(w, r, "friends.accept:"+from, nil, func(c *call) (idempotency.Result, error) {
		n, err := count(c.ctx, c.tx, `SELECT count(*) FROM friend_requests WHERE game_id=? AND from_player=? AND to_player=?`, c.gid, from, c.pid)
		if err != nil {
			return idempotency.Result{}, err
		}
		if n == 0 {
			return idempotency.Result{}, httpx.NotFound("request not found")
		}
		if err := m.makeFriends(c, c.pid, from); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Body: map[string]any{"status": "accepted", "player_id": from}}, nil
	})
}

func (m *Module) deleteRequest(w http.ResponseWriter, r *http.Request, op string, incoming bool) error {
	other, err := pathPlayer(r)
	if err != nil {
		return err
	}
	return m.mutate(w, r, op+":"+other, nil, func(c *call) (idempotency.Result, error) {
		from, to := c.pid, other
		if incoming {
			from, to = other, c.pid
		}
		res, err := c.tx.ExecContext(c.ctx, `DELETE FROM friend_requests WHERE game_id=? AND from_player=? AND to_player=?`, c.gid, from, to)
		if err != nil {
			return idempotency.Result{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return idempotency.Result{}, httpx.NotFound("request not found")
		}
		return idempotency.Result{Body: map[string]any{"status": op, "player_id": other}}, nil
	})
}

func (m *Module) declineRequest(w http.ResponseWriter, r *http.Request) error {
	return m.deleteRequest(w, r, "declined", true)
}

func (m *Module) cancelRequest(w http.ResponseWriter, r *http.Request) error {
	return m.deleteRequest(w, r, "cancelled", false)
}

func (m *Module) removeFriend(w http.ResponseWriter, r *http.Request) error {
	other, err := pathPlayer(r)
	if err != nil {
		return err
	}
	return m.mutate(w, r, "friends.remove:"+other, nil, func(c *call) (idempotency.Result, error) {
		res, err := c.tx.ExecContext(c.ctx, `DELETE FROM friendships WHERE game_id=? AND ((player_id=? AND friend_id=?) OR (player_id=? AND friend_id=?))`, c.gid, c.pid, other, other, c.pid)
		if err != nil {
			return idempotency.Result{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return idempotency.Result{}, httpx.NotFound("friend not found")
		}
		return idempotency.Result{Body: map[string]any{"removed": true, "player_id": other}}, nil
	})
}

// ---- blocks ----

func (m *Module) listBlocks(w http.ResponseWriter, r *http.Request) error {
	gid, pid := m.ident(r)
	rows, err := m.DB.Read.QueryContext(r.Context(), `SELECT blocked_id, created_at FROM player_blocks WHERE game_id=? AND player_id=? ORDER BY created_at, blocked_id LIMIT ?`, gid, pid, m.MaxBlocks)
	if err != nil {
		return err
	}
	defer rows.Close()
	type blk struct {
		PlayerID string `json:"player_id"`
		At       int64  `json:"at"`
	}
	out := []blk{}
	for rows.Next() {
		var b blk
		if err := rows.Scan(&b.PlayerID, &b.At); err != nil {
			return err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"blocks": out})
	return nil
}

// block removes friendship and pending requests in both directions and records
// the block, all in one transaction.
func (m *Module) block(w http.ResponseWriter, r *http.Request) error {
	target, err := pathPlayer(r)
	if err != nil {
		return err
	}
	return m.mutate(w, r, "blocks.add:"+target, nil, func(c *call) (idempotency.Result, error) {
		if target == c.pid {
			return idempotency.Result{}, httpx.BadRequest("cannot block yourself")
		}
		if err := game.EnsurePlayer(c.ctx, c.tx, c.gid, target); err != nil {
			return idempotency.Result{}, errNotFound
		}
		have, err := count(c.ctx, c.tx, `SELECT count(*) FROM player_blocks WHERE game_id=? AND player_id=? AND blocked_id=?`, c.gid, c.pid, target)
		if err != nil {
			return idempotency.Result{}, err
		}
		if have == 0 {
			if n, err := count(c.ctx, c.tx, `SELECT count(*) FROM player_blocks WHERE game_id=? AND player_id=?`, c.gid, c.pid); err != nil {
				return idempotency.Result{}, err
			} else if n >= m.MaxBlocks {
				return idempotency.Result{}, httpx.Conflict("block_limit", "block list is full")
			}
			if _, err := c.tx.ExecContext(c.ctx, `INSERT INTO player_blocks(game_id,player_id,blocked_id,created_at) VALUES(?,?,?,?)`, c.gid, c.pid, target, c.now); err != nil {
				return idempotency.Result{}, err
			}
		}
		if _, err := c.tx.ExecContext(c.ctx, `DELETE FROM friendships WHERE game_id=? AND ((player_id=? AND friend_id=?) OR (player_id=? AND friend_id=?))`, c.gid, c.pid, target, target, c.pid); err != nil {
			return idempotency.Result{}, err
		}
		if _, err := c.tx.ExecContext(c.ctx, `DELETE FROM friend_requests WHERE game_id=? AND ((from_player=? AND to_player=?) OR (from_player=? AND to_player=?))`, c.gid, c.pid, target, target, c.pid); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Body: map[string]any{"blocked": true, "player_id": target}}, nil
	})
}

func (m *Module) unblock(w http.ResponseWriter, r *http.Request) error {
	target, err := pathPlayer(r)
	if err != nil {
		return err
	}
	return m.mutate(w, r, "blocks.remove:"+target, nil, func(c *call) (idempotency.Result, error) {
		if _, err := c.tx.ExecContext(c.ctx, `DELETE FROM player_blocks WHERE game_id=? AND player_id=? AND blocked_id=?`, c.gid, c.pid, target); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Body: map[string]any{"blocked": false, "player_id": target}}, nil
	})
}
