// Package social is GameKit G13: profiles and privacy, friends, blocks,
// invites and share attribution. Identity (game_id, player_id) always comes from
// the session. Blocked or hidden players are indistinguishable from missing
// ones (uniform 404) so the API cannot be used to probe who exists.
package social

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
	"github.com/pillar/gostack/modules/ratelimit"
)

// Module serves the social API. Zero options take the documented defaults.
type Module struct {
	Kit *game.Kit
	DB  *sqlite.DB
	Now func() time.Time

	MaxFriends           int           // friends per player (default 100)
	MaxPendingRequests   int           // outgoing pending requests per player (default 50)
	MaxBlocks            int           // blocks per player (default 200)
	MaxInvites           int           // invite codes per player (default 5)
	MaxAcceptsPerInviter int           // attributed invitees per inviter (default 50)
	InviteWindow         time.Duration // an invitee must be created within this window (default 24h)
	InviterGrant         map[string]int64
	InviteeGrant         map[string]int64
	RequestsPerMinute    int // per-player limit on friend requests, invites and shares (default 30)

	once    sync.Once
	limiter *ratelimit.Limiter
}

func (m *Module) init() {
	m.once.Do(func() {
		if m.Now == nil {
			m.Now = time.Now
		}
		def := func(p *int, v int) {
			if *p <= 0 {
				*p = v
			}
		}
		def(&m.MaxFriends, 100)
		def(&m.MaxPendingRequests, 50)
		def(&m.MaxBlocks, 200)
		def(&m.MaxInvites, 5)
		def(&m.MaxAcceptsPerInviter, 50)
		def(&m.RequestsPerMinute, 30)
		if m.InviteWindow <= 0 {
			m.InviteWindow = 24 * time.Hour
		}
		m.limiter = ratelimit.PerMinute(m.RequestsPerMinute, 100_000)
	})
}

// Routes registers the social API under /v1/games/{game}.
func (m *Module) Routes(mux *http.ServeMux) {
	m.init()
	h := func(p string, f httpx.HandlerFunc) { m.Kit.Handle(mux, p, game.Player, f) }
	h("GET /social/profile", m.getProfile)
	h("PUT /social/profile", m.putProfile)

	h("GET /friends", m.listFriends)
	h("DELETE /friends/{player_id}", m.removeFriend)
	h("POST /friends/requests", m.sendRequest)
	h("GET /friends/requests", m.listRequests)
	h("POST /friends/requests/{player_id}/accept", m.acceptRequest)
	h("POST /friends/requests/{player_id}/decline", m.declineRequest)
	h("DELETE /friends/requests/{player_id}", m.cancelRequest)
	h("GET /friends/boards/{board}", m.friendBoard)

	h("GET /blocks", m.listBlocks)
	h("POST /blocks/{player_id}", m.block)
	h("DELETE /blocks/{player_id}", m.unblock)

	h("POST /invites", m.createInvite)
	h("GET /invites", m.listInvites)
	h("POST /invites/{code}/accept", m.acceptInvite)

	h("POST /shares", m.createShare)
	h("GET /shares/{id}", m.getShare)
	h("POST /shares/{id}/open", m.openShare)
}

// call is the identity and transaction of one idempotent mutation.
type call struct {
	ctx context.Context
	tx  *sql.Tx
	gid string
	pid string
	now int64
}

func (m *Module) ident(r *http.Request) (gid, pid string) {
	s := game.CurrentSession(r.Context())
	return s.GameID, s.PlayerID
}

// mutate runs fn in a write transaction under the request's Idempotency-Key.
func (m *Module) mutate(w http.ResponseWriter, r *http.Request, op string, body []byte, fn func(c *call) (idempotency.Result, error)) error {
	ctx := r.Context()
	gid, pid := m.ident(r)
	status, resp, replayed, err := idempotency.Do(ctx, m.DB, "social:"+gid+":"+pid, r.Header.Get("Idempotency-Key"), op, body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			return fn(&call{ctx: ctx, tx: tx, gid: gid, pid: pid, now: m.Now().UnixMilli()})
		})
	if err != nil {
		return game.MapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

func (m *Module) limit(r *http.Request, kind string) error {
	gid, pid := m.ident(r)
	return m.Kit.RateLimit(m.limiter, kind+":"+gid+":"+pid)
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var errNotFound = httpx.NotFound("player not found")

func pathPlayer(r *http.Request) (string, error) {
	id := r.PathValue("player_id")
	if !idRe.MatchString(id) {
		return "", errNotFound
	}
	return id, nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func count(ctx context.Context, q queryer, query string, args ...any) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// blocked reports whether either player blocks the other.
func blocked(ctx context.Context, q queryer, gid, a, b string) (bool, error) {
	n, err := count(ctx, q, `SELECT count(*) FROM player_blocks WHERE game_id=? AND ((player_id=? AND blocked_id=?) OR (player_id=? AND blocked_id=?))`, gid, a, b, b, a)
	return n > 0, err
}

// ---- profiles ----

type privacy struct {
	Searchable         bool   `json:"searchable"`
	FriendRequests     string `json:"friend_requests"`
	ShowOnFriendBoards bool   `json:"show_on_friend_boards"`
}

type profile struct {
	PlayerID    string  `json:"player_id"`
	DisplayName string  `json:"display_name"`
	FriendCode  string  `json:"friend_code"`
	Privacy     privacy `json:"privacy"`
}

const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func randomCode(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

func scanProfile(row *sql.Row, pid string) (profile, error) {
	p := profile{PlayerID: pid}
	var s, b int
	err := row.Scan(&p.DisplayName, &p.FriendCode, &s, &p.Privacy.FriendRequests, &b)
	p.Privacy.Searchable, p.Privacy.ShowOnFriendBoards = s == 1, b == 1
	return p, err
}

const profileCols = `SELECT display_name, friend_code, searchable, friend_requests, show_on_friend_boards FROM social_profiles WHERE game_id=? AND player_id=?`

// ensureProfile returns the player's profile, creating it (with a fresh friend
// code) on first use.
func ensureProfile(ctx context.Context, tx *sql.Tx, gid, pid string, now int64) (profile, error) {
	p, err := scanProfile(tx.QueryRowContext(ctx, profileCols, gid, pid), pid)
	if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	for i := 0; i < 10; i++ {
		code := randomCode(8)
		n, err := count(ctx, tx, `SELECT count(*) FROM social_profiles WHERE game_id=? AND friend_code=?`, gid, code)
		if err != nil {
			return p, err
		}
		if n > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO social_profiles(game_id,player_id,friend_code,created_at,updated_at) VALUES(?,?,?,?,?)`, gid, pid, code, now, now); err != nil {
			return p, err
		}
		return scanProfile(tx.QueryRowContext(ctx, profileCols, gid, pid), pid)
	}
	return p, errors.New("friend code space exhausted")
}

func (m *Module) getProfile(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	gid, pid := m.ident(r)
	p, err := scanProfile(m.DB.Read.QueryRowContext(ctx, profileCols, gid, pid), pid)
	if errors.Is(err, sql.ErrNoRows) {
		err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			var e error
			p, e = ensureProfile(ctx, tx, gid, pid, m.Now().UnixMilli())
			return e
		})
	}
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, p)
	return nil
}

// validName enforces 1-24 characters: letters, digits, space, '_', '-', '.'.
func validName(s string) bool {
	if s != strings.TrimSpace(s) || s == "" || utf8.RuneCountInString(s) > 24 {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (m *Module) putProfile(w http.ResponseWriter, r *http.Request) error {
	body, err := game.ReadBody(r, 2048)
	if err != nil {
		return err
	}
	var req struct {
		DisplayName *string `json:"display_name"`
		Privacy     *struct {
			Searchable         *bool   `json:"searchable"`
			FriendRequests     *string `json:"friend_requests"`
			ShowOnFriendBoards *bool   `json:"show_on_friend_boards"`
		} `json:"privacy"`
	}
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	if req.DisplayName != nil && !validName(*req.DisplayName) {
		return httpx.BadRequest("display_name must be 1-24 characters: letters, digits, space, _ - . (no leading/trailing space)")
	}
	if req.Privacy != nil && req.Privacy.FriendRequests != nil && *req.Privacy.FriendRequests != "everyone" && *req.Privacy.FriendRequests != "nobody" {
		return httpx.BadRequest("privacy.friend_requests must be everyone or nobody")
	}
	return m.mutate(w, r, "profile.put", body, func(c *call) (idempotency.Result, error) {
		p, err := ensureProfile(c.ctx, c.tx, c.gid, c.pid, c.now)
		if err != nil {
			return idempotency.Result{}, err
		}
		if req.DisplayName != nil {
			p.DisplayName = *req.DisplayName
		}
		if pr := req.Privacy; pr != nil {
			if pr.Searchable != nil {
				p.Privacy.Searchable = *pr.Searchable
			}
			if pr.FriendRequests != nil {
				p.Privacy.FriendRequests = *pr.FriendRequests
			}
			if pr.ShowOnFriendBoards != nil {
				p.Privacy.ShowOnFriendBoards = *pr.ShowOnFriendBoards
			}
		}
		_, err = c.tx.ExecContext(c.ctx, `UPDATE social_profiles SET display_name=?, searchable=?, friend_requests=?, show_on_friend_boards=?, updated_at=? WHERE game_id=? AND player_id=?`,
			p.DisplayName, b2i(p.Privacy.Searchable), p.Privacy.FriendRequests, b2i(p.Privacy.ShowOnFriendBoards), c.now, c.gid, c.pid)
		return idempotency.Result{Body: p}, err
	})
}
