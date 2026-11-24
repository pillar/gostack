package match

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/pillar/gostack/core/httpx"
)

type waiting struct {
	id      string
	player  string
	rating  int64
	created int64
}

// window is the allowed rating distance for a ticket that has waited so long.
func window(q queueDef, t waiting, now int64) int64 {
	w := q.RatingWindow + q.WidenPerSec*((now-t.created)/1000)
	if q.MaxWindow > 0 && w > q.MaxWindow {
		w = q.MaxWindow
	}
	return w
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// groupTickets forms rooms from tickets ordered oldest first. The oldest
// unmatched ticket anchors a group and takes the closest ratings as long as the
// group's rating spread stays within the narrowest member window.
func groupTickets(q queueDef, pool []waiting, now int64) [][]waiting {
	used := make([]bool, len(pool))
	var groups [][]waiting
	for i, a := range pool {
		if used[i] {
			continue
		}
		type cand struct {
			idx  int
			dist int64
		}
		var cands []cand
		for j := range pool {
			if j != i && !used[j] {
				cands = append(cands, cand{j, abs(pool[j].rating - a.rating)})
			}
		}
		sort.SliceStable(cands, func(x, y int) bool { return cands[x].dist < cands[y].dist })
		group := []int{i}
		lo, hi, minW := a.rating, a.rating, window(q, a, now)
		for _, c := range cands {
			if len(group) == q.Size {
				break
			}
			t := pool[c.idx]
			nlo, nhi, nw := min(lo, t.rating), max(hi, t.rating), min(minW, window(q, t, now))
			if nhi-nlo <= nw {
				group = append(group, c.idx)
				lo, hi, minW = nlo, nhi, nw
			}
		}
		if len(group) < q.Size {
			continue
		}
		g := make([]waiting, 0, len(group))
		for _, idx := range group {
			used[idx] = true
			g = append(g, pool[idx])
		}
		groups = append(groups, g)
	}
	return groups
}

// MatchOnce runs one matching pass over every configured queue and returns the
// number of rooms created. Tickets are read and consumed inside one write
// transaction per queue, so a ticket can only ever join a single room even when
// several passes run concurrently.
func (m *Module) MatchOnce(ctx context.Context) (int, error) {
	m.init()
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT game_id, `+queueCols+` FROM match_queues ORDER BY game_id, queue_id`)
	if err != nil {
		return 0, err
	}
	type gq struct {
		game string
		q    queueDef
	}
	var queues []gq
	for rows.Next() {
		var x gq
		if err := rows.Scan(&x.game, &x.q.ID, &x.q.Size, &x.q.RatingWindow, &x.q.WidenPerSec, &x.q.MaxWindow, &x.q.TTL, &x.q.Board); err != nil {
			rows.Close()
			return 0, err
		}
		queues = append(queues, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	total := 0
	var errs []error
	for _, x := range queues {
		n, err := m.matchQueue(ctx, x.game, x.q)
		total += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

func (m *Module) matchQueue(ctx context.Context, gid string, q queueDef) (int, error) {
	rooms := 0
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		rooms = 0 // fn may be re-run on SQLITE_BUSY
		now := m.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE match_tickets SET status='expired', updated_at=? WHERE game_id=? AND queue_id=? AND status='waiting' AND expires_at<=?`, now, gid, q.ID, now); err != nil {
			return err
		}
		rs, err := tx.QueryContext(ctx, `SELECT ticket_id, player_id, rating, created_at FROM match_tickets WHERE game_id=? AND queue_id=? AND status='waiting' ORDER BY created_at, ticket_id LIMIT ?`, gid, q.ID, m.MaxMatchBatch)
		if err != nil {
			return err
		}
		var pool []waiting
		for rs.Next() {
			var t waiting
			if err := rs.Scan(&t.id, &t.player, &t.rating, &t.created); err != nil {
				rs.Close()
				return err
			}
			pool = append(pool, t)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		if len(pool) < q.Size {
			return nil
		}
		for _, g := range groupTickets(q, pool, now) {
			room := httpx.RandomID("rm_")
			if _, err := tx.ExecContext(ctx, `INSERT INTO match_rooms(room_id,game_id,queue_id,status,lease_until,created_at,updated_at) VALUES(?,?,?,'open',?,?,?)`,
				room, gid, q.ID, now+m.ClaimTimeout.Milliseconds(), now, now); err != nil {
				return err
			}
			for _, t := range g {
				// Single use: the ticket must still be waiting, or the whole pass rolls back.
				res, err := tx.ExecContext(ctx, `UPDATE match_tickets SET status='matched', room_id=?, updated_at=? WHERE ticket_id=? AND status='waiting'`, room, now, t.id)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n != 1 {
					return errors.New("ticket " + t.id + " was not waiting")
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO match_room_members(room_id,game_id,player_id,ticket_id,state,updated_at) VALUES(?,?,?,?,'assigned',?)`,
					room, gid, t.player, t.id, now); err != nil {
					return err
				}
			}
			rooms++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rooms, nil
}

// Sweep expires tickets past their TTL and abandons rooms whose claim window or
// lease ran out; their players see ticket status 'abandoned'.
func (m *Module) Sweep(ctx context.Context) error {
	m.init()
	return m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		now := m.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE match_tickets SET status='expired', updated_at=? WHERE status='waiting' AND expires_at<=?`, now, now); err != nil {
			return err
		}
		rs, err := tx.QueryContext(ctx, `SELECT room_id FROM match_rooms WHERE status IN ('open','active') AND lease_until<=?`, now)
		if err != nil {
			return err
		}
		var ids []string
		for rs.Next() {
			var id string
			if err := rs.Scan(&id); err != nil {
				rs.Close()
				return err
			}
			ids = append(ids, id)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if err := abandonRoom(ctx, tx, id, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// purge deletes old terminal tickets and abandoned rooms. Settled rooms and
// their settlements are kept as the idempotency and audit record.
func (m *Module) purge(ctx context.Context) error {
	return m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		cut := m.Now().Add(-m.Retention).UnixMilli()
		for _, q := range []string{
			`DELETE FROM match_tickets WHERE status IN ('cancelled','expired','abandoned','done') AND updated_at<?`,
			`DELETE FROM match_room_members WHERE room_id IN (SELECT room_id FROM match_rooms WHERE status='abandoned' AND updated_at<?)`,
			`DELETE FROM match_rooms WHERE status='abandoned' AND updated_at<?`,
		} {
			if _, err := tx.ExecContext(ctx, q, cut); err != nil {
				return err
			}
		}
		return nil
	})
}
