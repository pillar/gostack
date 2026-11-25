package match_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/kits/game/match"
)

const cfg = `{"boards":{"score":{"order":"desc","mode":"best","period":"none","min_score":0,"max_score":1000,"quarantine_above":900}}}`

type fixture struct {
	e      *gametest.Env
	mod    *match.Module
	offset atomic.Int64 // ms added to the module clock
	battle string       // battle key, tenant default
	other  string       // battle key, tenant "other"
}

func setup(t *testing.T) *fixture {
	f := &fixture{}
	f.e = gametest.New(t, gametest.Options{Config: cfg, Routes: func(mux *http.ServeMux, a *app.App) {
		f.mod = &match.Module{Kit: a.Game, DB: a.DB, TicketsPerMinute: 100000, Now: func() time.Time {
			return time.Now().Add(time.Duration(f.offset.Load()) * time.Millisecond)
		}}
		f.mod.Routes(mux)
	}})
	ctx := context.Background()
	keys := auth.APIKeys{DB: f.e.App.DB}
	var err error
	if _, f.battle, err = keys.Create(ctx, "default", "battle", []string{"battle"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, f.other, err = keys.Create(ctx, "other", "battle", []string{"battle", "admin"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := game.CreateGame(ctx, f.e.App.DB, "other", "gamma", "gamma", []string{"https://gamma.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := game.PublishConfig(ctx, f.e.App.DB, "gamma", []byte(cfg), "test", ""); err != nil {
		t.Fatal(err)
	}
	f.queue("alpha", "duel", `{"size":2,"rating_window":50,"widen_per_sec":10,"ttl":600,"board":"score"}`)
	return f
}

func (f *fixture) advance(d time.Duration) { f.offset.Add(d.Milliseconds()) }

func (f *fixture) queue(g, name, body string) {
	f.e.T.Helper()
	tok := f.e.AdminToken
	if g == "gamma" {
		tok = f.other
	}
	f.e.Expect(f.e.Do("PUT", "/v1/admin/games/"+g+"/match/queues/"+name, tok, body), 200)
}

func (f *fixture) expect(r gametest.Resp, status int, code string) gametest.Resp {
	f.e.T.Helper()
	f.e.Expect(r, status)
	if code != "" && r.Code() != code {
		f.e.T.Fatalf("code %q want %q: %s", r.Code(), code, r.Raw)
	}
	return r
}

func (f *fixture) ticket(p gametest.Player, queue string, rating int) string {
	f.e.T.Helper()
	r := f.expect(f.e.Do("POST", "/v1/games/alpha/match/tickets", p.Token, map[string]any{"queue": queue, "attributes": map[string]int{"rating": rating}}), 201, "")
	return r.Body["ticket_id"].(string)
}

func (f *fixture) status(p gametest.Player, id string) gametest.Resp {
	return f.e.Do("GET", "/v1/games/alpha/match/tickets/"+id, p.Token, nil)
}

func (f *fixture) matchOnce() int {
	f.e.T.Helper()
	n, err := f.mod.MatchOnce(context.Background())
	if err != nil {
		f.e.T.Fatal(err)
	}
	return n
}

// pair creates two players with tickets and matches them; returns players, tickets and room id.
func (f *fixture) pair() ([]gametest.Player, []string, string) {
	f.e.T.Helper()
	ps := []gametest.Player{f.e.Guest("alpha"), f.e.Guest("alpha")}
	ts := []string{f.ticket(ps[0], "duel", 100), f.ticket(ps[1], "duel", 100)}
	if n := f.matchOnce(); n != 1 {
		f.e.T.Fatalf("rooms %d", n)
	}
	r := f.expect(f.status(ps[0], ts[0]), 200, "")
	return ps, ts, r.Body["room"].(map[string]any)["room_id"].(string)
}

func (f *fixture) claim(token string, body any) gametest.Resp {
	return f.e.Do("POST", "/v1/match/rooms/claim", token, body)
}

func (f *fixture) claimed(connURL string) (room string, tok string) {
	f.e.T.Helper()
	r := f.expect(f.claim(f.battle, map[string]any{"server_id": "bs1", "connection": map[string]string{"url": connURL}}), 200, "")
	return r.Body["room_id"].(string), r.Body["token"].(string)
}

func (f *fixture) room(id, op string, token string, body any) gametest.Resp {
	return f.e.Do("POST", "/v1/match/rooms/"+id+"/"+op, token, body)
}

func TestQueueAdmin(t *testing.T) {
	f := setup(t)
	p := f.e.Guest("alpha")
	url := "/v1/admin/games/alpha/match/queues/q1"
	for _, bad := range []string{
		`{}`, `{"size":1,"rating_window":1,"widen_per_sec":0,"ttl":60}`, `{"size":2,"rating_window":-1,"widen_per_sec":0,"ttl":60}`,
		`{"size":2,"rating_window":1,"widen_per_sec":0,"ttl":1}`, `{"size":2,"rating_window":1,"widen_per_sec":0,"ttl":60,"board":"nope"}`,
		`{"size":2,"rating_window":1,"widen_per_sec":0,"ttl":60,"x":1}`,
	} {
		f.expect(f.e.Do("PUT", url, f.e.AdminToken, bad), 400, "")
	}
	f.expect(f.e.Do("PUT", url, p.Token, `{"size":2,"rating_window":1,"widen_per_sec":0,"ttl":60}`), 401, "")
	f.expect(f.e.Do("PUT", url, f.battle, `{"size":2,"rating_window":1,"widen_per_sec":0,"ttl":60}`), 403, "")
	f.expect(f.e.Do("PUT", "/v1/admin/games/gamma/match/queues/q1", f.e.AdminToken, `{"size":2,"rating_window":1,"widen_per_sec":0,"ttl":60}`), 404, "")
	f.queue("alpha", "q1", `{"size":4,"rating_window":10,"widen_per_sec":1,"ttl":60}`)
	f.queue("alpha", "q1", `{"size":3,"rating_window":10,"widen_per_sec":1,"ttl":60,"max_window":100}`) // update
	r := f.expect(f.e.Do("GET", "/v1/admin/games/alpha/match/queues", f.e.AdminToken, nil), 200, "")
	qs := r.Body["queues"].([]any)
	if len(qs) != 2 || qs[1].(map[string]any)["size"] != float64(3) {
		t.Fatalf("%s", r.Raw)
	}
	if r := f.e.Do("GET", "/v1/admin/games/beta/match/queues", f.e.AdminToken, nil); len(r.Body["queues"].([]any)) != 0 {
		t.Fatalf("queues leak across games: %s", r.Raw)
	}
}

func TestTicketLifecycle(t *testing.T) {
	f := setup(t)
	a, b := f.e.Guest("alpha"), f.e.Guest("alpha")
	f.expect(f.e.Do("POST", "/v1/games/alpha/match/tickets", a.Token, map[string]string{"queue": "nope"}), 404, "")
	f.expect(f.e.Do("POST", "/v1/games/alpha/match/tickets", a.Token, map[string]any{"queue": "duel", "attributes": map[string]int{"rating": 99999999}}), 400, "")
	f.expect(f.e.Do("POST", "/v1/games/alpha/match/tickets", "", map[string]string{"queue": "duel"}), 401, "")
	// queues are per game
	bp := f.e.Guest("beta")
	f.expect(f.e.Do("POST", "/v1/games/beta/match/tickets", bp.Token, map[string]string{"queue": "duel"}), 404, "")

	body := map[string]any{"queue": "duel", "attributes": map[string]int{"rating": 10}}
	r1 := f.expect(f.e.Do("POST", "/v1/games/alpha/match/tickets", a.Token, body, "Idempotency-Key", "t1"), 201, "")
	r2 := f.expect(f.e.Do("POST", "/v1/games/alpha/match/tickets", a.Token, body, "Idempotency-Key", "t1"), 201, "")
	if r2.Header.Get("Idempotent-Replayed") != "true" || string(r1.Raw) != string(r2.Raw) {
		t.Fatal("replay expected")
	}
	id := r1.Body["ticket_id"].(string)
	f.expect(f.e.Do("POST", "/v1/games/alpha/match/tickets", a.Token, body), 409, "ticket_active")
	if s := f.expect(f.status(a, id), 200, ""); s.Body["status"] != "waiting" || s.Body["queue"] != "duel" {
		t.Fatalf("%s", s.Raw)
	}
	f.expect(f.status(b, id), 404, "") // not someone else's ticket
	f.expect(f.e.Do("DELETE", "/v1/games/alpha/match/tickets/"+id, b.Token, nil), 404, "")
	f.expect(f.e.Do("DELETE", "/v1/games/alpha/match/tickets/"+id, a.Token, nil), 200, "")
	f.expect(f.e.Do("DELETE", "/v1/games/alpha/match/tickets/"+id, a.Token, nil), 200, "")
	if f.status(a, id).Body["status"] != "cancelled" {
		t.Fatal("not cancelled")
	}
	if f.matchOnce() != 0 {
		t.Fatal("cancelled ticket must not match")
	}
	// TTL: an expired ticket reports expired and does not block a new one.
	id2 := f.ticket(a, "duel", 10)
	f.advance(601 * time.Second)
	if f.status(a, id2).Body["status"] != "expired" {
		t.Fatal("ticket should be expired")
	}
	f.ticket(a, "duel", 10)
	f.expect(f.e.Do("DELETE", "/v1/games/alpha/match/tickets/"+id2, a.Token, nil), 409, "ticket_closed")
}

func TestMatchingWindow(t *testing.T) {
	f := setup(t)
	a, b, c := f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha")
	ta, tb, tc := f.ticket(a, "duel", 100), f.ticket(b, "duel", 500), f.ticket(c, "duel", 130)
	if f.matchOnce() != 1 {
		t.Fatal("a and c are within the window")
	}
	if f.status(a, ta).Body["status"] != "matched" || f.status(c, tc).Body["status"] != "matched" || f.status(b, tb).Body["status"] != "waiting" {
		t.Fatal("wrong pairing")
	}
	// Window widens with waiting time: 400 apart needs 350+ window... 50 + 10/s * 40s.
	d := f.e.Guest("alpha")
	td := f.ticket(d, "duel", 100)
	_ = td
	if f.matchOnce() != 0 {
		t.Fatal("b(500) and d(100) are too far apart")
	}
	f.advance(40 * time.Second)
	if f.matchOnce() != 1 {
		t.Fatal("window should have widened")
	}
	if f.status(b, tb).Body["status"] != "matched" {
		t.Fatal("b should be matched")
	}
	// max_window caps widening
	f.queue("alpha", "capped", `{"size":2,"rating_window":0,"widen_per_sec":1000,"max_window":10,"ttl":600}`)
	e1, e2 := f.e.Guest("alpha"), f.e.Guest("alpha")
	f.ticket(e1, "capped", 0)
	f.ticket(e2, "capped", 500)
	f.advance(60 * time.Second)
	if f.matchOnce() != 0 {
		t.Fatal("max_window must cap widening")
	}
}

func TestMatchGroupOfThree(t *testing.T) {
	f := setup(t)
	f.queue("alpha", "trio", `{"size":3,"rating_window":100,"widen_per_sec":0,"ttl":60}`)
	var ps []gametest.Player
	for _, r := range []int{0, 50, 100, 400} {
		p := f.e.Guest("alpha")
		ps = append(ps, p)
		f.ticket(p, "trio", r)
	}
	if f.matchOnce() != 1 {
		t.Fatal("expected one room")
	}
	var matched int
	for _, p := range ps {
		var n int
		f.e.App.DB.Read.QueryRow(`SELECT count(*) FROM match_tickets WHERE player_id=? AND status='matched'`, p.ID).Scan(&n)
		matched += n
	}
	if matched != 3 {
		t.Fatalf("matched %d", matched)
	}
}

// Concurrent matching passes must put each ticket into exactly one room.
func TestConcurrentMatchOnce(t *testing.T) {
	f := setup(t)
	const players = 24
	for i := 0; i < players; i++ {
		f.ticket(f.e.Guest("alpha"), "duel", 100)
	}
	var wg sync.WaitGroup
	var rooms atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := f.mod.MatchOnce(context.Background())
			if err != nil {
				t.Error(err)
			}
			rooms.Add(int32(n))
		}()
	}
	wg.Wait()
	if rooms.Load() != players/2 {
		t.Fatalf("rooms created %d want %d", rooms.Load(), players/2)
	}
	db := f.e.App.DB.Read
	var members, distinct, nrooms, waiting int
	db.QueryRow(`SELECT count(*), count(DISTINCT player_id) FROM match_room_members`).Scan(&members, &distinct)
	db.QueryRow(`SELECT count(*) FROM match_rooms`).Scan(&nrooms)
	db.QueryRow(`SELECT count(*) FROM match_tickets WHERE status='waiting'`).Scan(&waiting)
	if members != players || distinct != players || nrooms != players/2 || waiting != 0 {
		t.Fatalf("members=%d distinct=%d rooms=%d waiting=%d", members, distinct, nrooms, waiting)
	}
	var bad int
	db.QueryRow(`SELECT count(*) FROM (SELECT room_id FROM match_room_members GROUP BY room_id HAVING count(*) <> 2)`).Scan(&bad)
	if bad != 0 {
		t.Fatal("a room has the wrong size")
	}
}

func TestRoomLifecycleAndSettlement(t *testing.T) {
	f := setup(t)
	ps, ts, roomID := f.pair()
	a, b := ps[0], ps[1]
	outsider := f.e.Guest("alpha")

	// matched but unclaimed: no connection info, ticket cannot be cancelled or duplicated
	s := f.expect(f.status(a, ts[0]), 200, "")
	rv := s.Body["room"].(map[string]any)
	if s.Body["status"] != "matched" || rv["status"] != "open" || rv["connection"] != nil || len(rv["members"].([]any)) != 2 {
		t.Fatalf("%s", s.Raw)
	}
	f.expect(f.e.Do("DELETE", "/v1/games/alpha/match/tickets/"+ts[0], a.Token, nil), 409, "already_matched")
	f.expect(f.e.Do("POST", "/v1/games/alpha/match/tickets", a.Token, map[string]string{"queue": "duel"}), 409, "ticket_active")

	// battle auth
	f.expect(f.claim("", map[string]any{"server_id": "bs1"}), 401, "")
	f.expect(f.claim(f.e.AdminToken, map[string]any{"server_id": "bs1"}), 403, "")
	f.expect(f.claim(a.Token, map[string]any{"server_id": "bs1"}), 401, "")
	f.expect(f.claim(f.battle, map[string]any{"server_id": ""}), 400, "")
	f.expect(f.claim(f.battle, map[string]any{"server_id": "bs1", "connection": "x"}), 400, "")
	f.expect(f.claim(f.other, map[string]any{"server_id": "bs9"}), 204, "") // other tenant sees nothing

	id, tok := f.claimed("wss://battle.example/r1")
	if id != roomID || tok == "" {
		t.Fatalf("claimed %s tok %v", id, tok)
	}
	f.expect(f.claim(f.battle, map[string]any{"server_id": "bs1"}), 204, "") // nothing left
	s = f.expect(f.status(b, ts[1]), 200, "")
	rv = s.Body["room"].(map[string]any)
	if rv["status"] != "active" || rv["connection"].(map[string]any)["url"] != "wss://battle.example/r1" {
		t.Fatalf("%s", s.Raw)
	}

	// fencing: wrong / zero / future token
	for _, bad := range []string{"", "mrt_guess", tok + "x"} {
		f.expect(f.room(roomID, "heartbeat", f.battle, map[string]any{"token": bad}), 409, "stale_token")
	}
	f.expect(f.room(roomID, "heartbeat", f.battle, map[string]any{"token": tok}), 200, "")
	f.expect(f.room(roomID, "heartbeat", f.e.AdminToken, map[string]any{"token": tok}), 403, "")
	f.expect(f.room("rm_unknown", "heartbeat", f.battle, map[string]any{"token": tok}), 404, "")

	// disconnect / rejoin
	st := func(p gametest.Player, state string) gametest.Resp {
		return f.e.Do("POST", "/v1/match/rooms/"+roomID+"/members/"+p.ID+"/state", f.battle, map[string]any{"token": tok, "state": state})
	}
	f.expect(st(a, "left"), 200, "")
	f.expect(st(outsider, "left"), 404, "")
	f.expect(st(a, "gone"), 400, "")
	got := f.status(a, ts[0]).Body["room"].(map[string]any)["members"].([]any)
	states := map[string]any{}
	for _, m := range got {
		states[m.(map[string]any)["player_id"].(string)] = m.(map[string]any)["state"]
	}
	if states[a.ID] != "left" || states[b.ID] != "assigned" {
		t.Fatalf("%v", states)
	}
	f.expect(st(a, "connected"), 200, "")

	// settlement validation: nothing is applied when any entry is invalid
	res := func(token string, results ...map[string]any) gametest.Resp {
		return f.room(roomID, "result", f.battle, map[string]any{"token": token, "results": results})
	}
	f.expect(res(tok, map[string]any{"player_id": outsider.ID}), 400, "not_a_member")
	f.expect(res(tok, map[string]any{"player_id": a.ID, "grants": map[string]int{"coin": -5}}), 400, "")
	f.expect(res(tok, map[string]any{"player_id": a.ID, "grants": map[string]int{"coin": 10}}, map[string]any{"player_id": b.ID, "score": 5000}), 400, "")
	f.expect(res(tok+"x", map[string]any{"player_id": a.ID}), 409, "stale_token")
	f.expect(f.room(roomID, "result", f.battle, map[string]any{"token": tok, "results": []any{}}), 400, "")
	if bal := f.e.Do("GET", "/v1/games/alpha/wallet", a.Token, nil).Body["balances"].(map[string]any); len(bal) != 0 {
		t.Fatalf("invalid settlement applied grants: %v", bal)
	}

	win := map[string]any{"player_id": a.ID, "score": 700, "grants": map[string]int{"coin": 10, "gem": 1}}
	lose := map[string]any{"player_id": b.ID, "grants": map[string]int{"coin": 3}}
	r1 := f.expect(res(tok, win, lose), 200, "")
	r2 := f.expect(res(tok, win, lose), 200, "")
	if r2.Header.Get("Idempotent-Replayed") != "true" || string(r1.Raw) != string(r2.Raw) {
		t.Fatalf("replay differs: %s vs %s", r1.Raw, r2.Raw)
	}
	f.expect(res(tok, map[string]any{"player_id": a.ID, "grants": map[string]int{"coin": 999}}), 409, "already_settled")
	f.expect(res(tok+"x", win, lose), 409, "stale_token")
	f.expect(f.room(roomID, "heartbeat", f.battle, map[string]any{"token": tok}), 409, "room_settled")

	bal := func(p gametest.Player) map[string]any {
		return f.e.Do("GET", "/v1/games/alpha/wallet", p.Token, nil).Body["balances"].(map[string]any)
	}
	if ba := bal(a); ba["coin"] != float64(10) || ba["gem"] != float64(1) || bal(b)["coin"] != float64(3) {
		t.Fatalf("balances a=%v b=%v", bal(a), bal(b))
	}
	if r := f.e.Do("GET", "/v1/games/alpha/boards/score", a.Token, nil); r.Body["me"].(map[string]any)["score"] != float64(700) {
		t.Fatalf("score not on board: %s", r.Raw)
	}
	if f.status(a, ts[0]).Body["status"] != "done" {
		t.Fatal("ticket should be done")
	}
	f.ticket(a, "duel", 100) // free to queue again
	if mm, err := game.Reconcile(context.Background(), f.e.App.DB.Read, "alpha"); err != nil || len(mm) != 0 {
		t.Fatalf("ledger mismatch %v %v", mm, err)
	}
}

func TestSettlementQuarantineAndNoBoard(t *testing.T) {
	f := setup(t)
	ps, _, roomID := f.pair()
	_, tok := f.claimed("x")
	r := f.expect(f.room(roomID, "result", f.battle, map[string]any{"token": tok, "results": []map[string]any{{"player_id": ps[0].ID, "score": 950}}}), 200, "")
	if r.Body["results"].([]any)[0].(map[string]any)["score_status"] != "quarantined" {
		t.Fatalf("%s", r.Raw)
	}
	if r := f.e.Do("GET", "/v1/games/alpha/boards/score", ps[0].Token, nil); r.Body["me"] != nil {
		t.Fatalf("quarantined score ranked: %s", r.Raw)
	}
	// a queue without a board rejects scores
	f.queue("alpha", "noboard", `{"size":2,"rating_window":50,"widen_per_sec":0,"ttl":600}`)
	a, b := f.e.Guest("alpha"), f.e.Guest("alpha")
	f.ticket(a, "noboard", 1)
	f.ticket(b, "noboard", 1)
	f.matchOnce()
	id, tok2 := f.claimed("x")
	f.expect(f.room(id, "result", f.battle, map[string]any{"token": tok2, "results": []map[string]any{{"player_id": a.ID, "score": 5}}}), 400, "no_board")
	f.expect(f.room(id, "result", f.battle, map[string]any{"token": tok2, "results": []map[string]any{{"player_id": a.ID}}}), 200, "")
}

func TestConcurrentSettlement(t *testing.T) {
	f := setup(t)
	ps, _, roomID := f.pair()
	_, tok := f.claimed("x")
	body := map[string]any{"token": tok, "results": []map[string]any{{"player_id": ps[0].ID, "grants": map[string]int{"coin": 7}}}}
	other := map[string]any{"token": tok, "results": []map[string]any{{"player_id": ps[1].ID, "grants": map[string]int{"coin": 9}}}}
	var wg sync.WaitGroup
	var ok, conflict atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := body
			if i%3 == 0 {
				b = other // a different result racing the first
			}
			switch f.room(roomID, "result", f.battle, b).Status {
			case 200:
				ok.Add(1)
			case 409:
				conflict.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load()+conflict.Load() != 10 || ok.Load() < 1 {
		t.Fatalf("ok=%d conflict=%d", ok.Load(), conflict.Load())
	}
	var n int
	f.e.App.DB.Read.QueryRow(`SELECT count(*) FROM game_ledger WHERE source_key LIKE 'match:%'`).Scan(&n)
	var settlements int
	f.e.App.DB.Read.QueryRow(`SELECT count(*) FROM match_settlements`).Scan(&settlements)
	if n != 1 || settlements != 1 {
		t.Fatalf("ledger rows %d settlements %d, want exactly one", n, settlements)
	}
}

func TestLeaseExpiryAbandonsRoom(t *testing.T) {
	f := setup(t)
	ps, ts, roomID := f.pair()
	_, tok := f.claimed("x")
	f.advance(20 * time.Second)
	f.expect(f.room(roomID, "heartbeat", f.battle, map[string]any{"token": tok}), 200, "") // extends the lease
	f.advance(20 * time.Second)                                                            // 40s total but only 20s since heartbeat
	f.expect(f.room(roomID, "heartbeat", f.battle, map[string]any{"token": tok}), 200, "")
	f.advance(31 * time.Second)
	// the player already sees the abandonment, before anything persists it
	if f.status(ps[0], ts[0]).Body["status"] != "abandoned" {
		t.Fatal("ticket should read as abandoned")
	}
	f.expect(f.room(roomID, "heartbeat", f.battle, map[string]any{"token": tok}), 409, "room_abandoned")
	f.expect(f.room(roomID, "result", f.battle, map[string]any{"token": tok, "results": []map[string]any{{"player_id": ps[0].ID, "grants": map[string]int{"coin": 5}}}}), 409, "room_abandoned")
	var status string
	f.e.App.DB.Read.QueryRow(`SELECT status FROM match_rooms WHERE room_id=?`, roomID).Scan(&status)
	if status != "abandoned" {
		t.Fatalf("abandonment not persisted: %s", status)
	}
	if f.status(ps[1], ts[1]).Body["status"] != "abandoned" {
		t.Fatal("ticket status")
	}
	f.ticket(ps[0], "duel", 1) // players are released
}

func TestSweepAbandonsUnclaimedRoomAndStaleTicketsAreReleased(t *testing.T) {
	f := setup(t)
	ps, ts, roomID := f.pair()
	f.advance(61 * time.Second) // ClaimTimeout 60s
	f.expect(f.claim(f.battle, map[string]any{"server_id": "late"}), 204, "")
	if err := f.mod.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	f.e.App.DB.Read.QueryRow(`SELECT status FROM match_rooms WHERE room_id=?`, roomID).Scan(&status)
	if status != "abandoned" || f.status(ps[0], ts[0]).Body["status"] != "abandoned" {
		t.Fatalf("room %s", status)
	}
	// Without a sweep, creating a ticket also releases a stale matched ticket.
	ps2, _, _ := f.pair()
	f.advance(61 * time.Second)
	f.ticket(ps2[0], "duel", 1)
}

func TestFencingTokensGrowAndAreRoomScoped(t *testing.T) {
	f := setup(t)
	_, _, r1 := f.pair()
	_, _, r2 := f.pair()
	id1, t1 := f.claimed("a")
	id2, t2 := f.claimed("b")
	if id1 != r1 || id2 != r2 || t2 == t1 {
		t.Fatalf("tokens %v %v rooms %s %s", t1, t2, id1, id2)
	}
	f.expect(f.room(r2, "heartbeat", f.battle, map[string]any{"token": t1}), 409, "stale_token")
	f.expect(f.room(r1, "heartbeat", f.battle, map[string]any{"token": t2}), 409, "stale_token")
	f.expect(f.room(r2, "heartbeat", f.battle, map[string]any{"token": t2}), 200, "")
}

func TestTenantIsolation(t *testing.T) {
	f := setup(t)
	f.queue("gamma", "duel", `{"size":2,"rating_window":50,"widen_per_sec":0,"ttl":600}`)
	var ps []gametest.Player
	for i := 0; i < 2; i++ {
		r := f.e.Expect(f.e.Do("POST", "/v1/games/gamma/auth/guest", "", "{}"), 201)
		p := gametest.Player{ID: r.Body["player_id"].(string), Token: r.Body["access_token"].(string)}
		f.e.Expect(f.e.Do("POST", "/v1/games/gamma/match/tickets", p.Token, map[string]string{"queue": "duel"}), 201)
		ps = append(ps, p)
	}
	if f.matchOnce() != 1 {
		t.Fatal("gamma room expected")
	}
	// the default-tenant battle key cannot claim or touch the other tenant's room
	f.expect(f.claim(f.battle, map[string]any{"server_id": "bs1"}), 204, "")
	var room string
	f.e.App.DB.Read.QueryRow(`SELECT room_id FROM match_rooms WHERE game_id='gamma'`).Scan(&room)
	f.expect(f.room(room, "heartbeat", f.battle, map[string]any{"token": "mrt_guess"}), 404, "")
	f.expect(f.room(room, "result", f.battle, map[string]any{"token": "mrt_guess", "results": []map[string]any{{"player_id": ps[0].ID}}}), 404, "")
	f.expect(f.e.Do("POST", "/v1/match/rooms/"+room+"/members/"+ps[0].ID+"/state", f.battle, map[string]any{"token": "mrt_guess", "state": "left"}), 404, "")
	// the owning tenant can
	r := f.expect(f.claim(f.other, map[string]any{"server_id": "bs2", "game_id": "gamma"}), 200, "")
	if r.Body["game_id"] != "gamma" {
		t.Fatalf("%s", r.Raw)
	}
	f.expect(f.room(room, "heartbeat", f.other, map[string]any{"token": r.Body["token"].(string)}), 200, "")
	// a player of alpha cannot read a gamma ticket (and vice versa): tickets are game/player scoped
	a := f.e.Guest("alpha")
	var tid string
	f.e.App.DB.Read.QueryRow(`SELECT ticket_id FROM match_tickets WHERE game_id='gamma' LIMIT 1`).Scan(&tid)
	f.expect(f.status(a, tid), 404, "")
}
