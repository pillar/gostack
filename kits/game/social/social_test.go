package social_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/kits/game/social"
)

const cfg = `{"boards":{"score":{"order":"desc","mode":"best","period":"none","min_score":0,"max_score":1000000}}}`

type fixture struct {
	e   *gametest.Env
	mod *social.Module
	now atomic.Int64 // ms offset added to the module clock
}

func setup(t *testing.T, tune func(*social.Module)) *fixture {
	f := &fixture{}
	f.e = gametest.New(t, gametest.Options{Config: cfg, Routes: func(mux *http.ServeMux, a *app.App) {
		f.mod = &social.Module{Kit: a.Game, DB: a.DB, Now: func() time.Time {
			return time.Now().Add(time.Duration(f.now.Load()) * time.Millisecond)
		}, RequestsPerMinute: 10000}
		if tune != nil {
			tune(f.mod)
		}
		f.mod.Routes(mux)
	}})
	return f
}

func (f *fixture) do(method, path string, p gametest.Player, body any, hdr ...string) gametest.Resp {
	return f.e.Do(method, "/v1/games/alpha"+path, p.Token, body, hdr...)
}

func (f *fixture) expect(r gametest.Resp, status int, code string) gametest.Resp {
	f.e.T.Helper()
	f.e.Expect(r, status)
	if code != "" && r.Code() != code {
		f.e.T.Fatalf("code %q want %q: %s", r.Code(), code, r.Raw)
	}
	return r
}

func (f *fixture) befriend(a, b gametest.Player) {
	f.e.T.Helper()
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": b.ID}), 201, "")
	f.expect(f.do("POST", "/friends/requests/"+a.ID+"/accept", b, nil), 200, "")
}

func friendIDs(r gametest.Resp) []string {
	var out []string
	for _, x := range r.Body["friends"].([]any) {
		out = append(out, x.(map[string]any)["player_id"].(string))
	}
	return out
}

func TestProfile(t *testing.T) {
	f := setup(t, nil)
	a := f.e.Guest("alpha")
	r := f.expect(f.do("GET", "/social/profile", a, nil), 200, "")
	code := r.Body["friend_code"].(string)
	if len(code) != 8 || r.Body["privacy"].(map[string]any)["friend_requests"] != "everyone" {
		t.Fatalf("%s", r.Raw)
	}
	if f.do("GET", "/social/profile", a, nil).Body["friend_code"] != code {
		t.Fatal("friend code must be stable")
	}
	r = f.expect(f.do("PUT", "/social/profile", a, `{"display_name":"Ann Lee_1","privacy":{"searchable":false,"friend_requests":"nobody"}}`), 200, "")
	pr := r.Body["privacy"].(map[string]any)
	if r.Body["display_name"] != "Ann Lee_1" || pr["searchable"] != false || pr["friend_requests"] != "nobody" || pr["show_on_friend_boards"] != true {
		t.Fatalf("%s", r.Raw)
	}
	for _, bad := range []string{
		`{"display_name":""}`, `{"display_name":" x"}`, `{"display_name":"abcdefghijklmnopqrstuvwxyz"}`,
		`{"display_name":"a<b>"}`, `{"display_name":"a\nb"}`, `{"privacy":{"friend_requests":"friends"}}`, `{"nope":1}`,
	} {
		f.expect(f.do("PUT", "/social/profile", a, bad), 400, "invalid_request")
	}
	f.expect(f.do("GET", "/social/profile", gametest.Player{}, nil), 401, "")
	// Profiles are per game.
	b := f.e.Guest("beta")
	if f.e.Do("GET", "/v1/games/alpha/social/profile", b.Token, nil).Status != 403 {
		t.Fatal("beta session must not read alpha profile")
	}
}

func TestFriendFlow(t *testing.T) {
	f := setup(t, nil)
	a, b, c := f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha")
	f.expect(f.do("PUT", "/social/profile", b, `{"display_name":"Bob"}`), 200, "")

	// self, unknown, cross-game, malformed
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": a.ID}), 400, "invalid_request")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": "plr_nope"}), 404, "not_found")
	other := f.e.Guest("beta")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": other.ID}), 404, "not_found")
	f.expect(f.do("POST", "/friends/requests", a, `{}`), 400, "")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": b.ID, "friend_code": "ABCDEFGH"}), 400, "")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"friend_code": "ZZZZZZZZ"}), 404, "not_found")

	// request, listings, duplicate
	idem := []string{"Idempotency-Key", "k1"}
	r1 := f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": b.ID}, idem...), 201, "")
	r2 := f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": b.ID}, idem...), 201, "")
	if r2.Header.Get("Idempotent-Replayed") != "true" || string(r1.Raw) != string(r2.Raw) {
		t.Fatalf("replay: %v %s", r2.Header, r2.Raw)
	}
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": b.ID}), 409, "already_requested")
	in := f.expect(f.do("GET", "/friends/requests", b, nil), 200, "")
	if len(in.Body["incoming"].([]any)) != 1 || len(in.Body["outgoing"].([]any)) != 0 {
		t.Fatalf("%s", in.Raw)
	}
	out := f.expect(f.do("GET", "/friends/requests", a, nil), 200, "")
	if len(out.Body["outgoing"].([]any)) != 1 || out.Body["outgoing"].([]any)[0].(map[string]any)["display_name"] != "Bob" {
		t.Fatalf("%s", out.Raw)
	}
	// only the recipient can accept; the sender cannot
	f.expect(f.do("POST", "/friends/requests/"+b.ID+"/accept", a, nil), 404, "")
	f.expect(f.do("POST", "/friends/requests/"+c.ID+"/accept", b, nil), 404, "")
	f.expect(f.do("POST", "/friends/requests/"+a.ID+"/accept", b, nil), 200, "")
	if ids := friendIDs(f.expect(f.do("GET", "/friends", a, nil), 200, "")); len(ids) != 1 || ids[0] != b.ID {
		t.Fatalf("a friends %v", ids)
	}
	if ids := friendIDs(f.expect(f.do("GET", "/friends", b, nil), 200, "")); len(ids) != 1 || ids[0] != a.ID {
		t.Fatalf("b friends %v", ids)
	}
	f.expect(f.do("POST", "/friends/requests", b, map[string]string{"player_id": a.ID}), 409, "already_friends")
	f.expect(f.do("POST", "/friends/requests/"+a.ID+"/accept", b, nil), 404, "") // request consumed

	// remove is symmetric
	f.expect(f.do("DELETE", "/friends/"+a.ID, b, nil), 200, "")
	f.expect(f.do("DELETE", "/friends/"+a.ID, b, nil), 404, "")
	if len(friendIDs(f.do("GET", "/friends", a, nil))) != 0 {
		t.Fatal("friendship must be removed on both sides")
	}

	// decline and cancel
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": c.ID}), 201, "")
	f.expect(f.do("POST", "/friends/requests/"+a.ID+"/decline", c, nil), 200, "")
	f.expect(f.do("POST", "/friends/requests/"+a.ID+"/decline", c, nil), 404, "")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": c.ID}), 201, "")
	f.expect(f.do("DELETE", "/friends/requests/"+c.ID, a, nil), 200, "")
	f.expect(f.do("POST", "/friends/requests/"+a.ID+"/accept", c, nil), 404, "")

	// crossing requests become a friendship
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": c.ID}), 201, "")
	r := f.expect(f.do("POST", "/friends/requests", c, map[string]string{"player_id": a.ID}), 200, "")
	if r.Body["status"] != "accepted" {
		t.Fatalf("%s", r.Raw)
	}
	if ids := friendIDs(f.do("GET", "/friends", a, nil)); len(ids) != 1 || ids[0] != c.ID {
		t.Fatalf("%v", ids)
	}
}

func TestFriendCodeAndPrivacy(t *testing.T) {
	f := setup(t, nil)
	a, b, c := f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha")
	code := f.do("GET", "/social/profile", b, nil).Body["friend_code"].(string)
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"friend_code": code}), 201, "")
	f.expect(f.do("DELETE", "/friends/requests/"+b.ID, a, nil), 200, "")

	// nobody -> 403 requests_disabled
	f.expect(f.do("PUT", "/social/profile", b, `{"privacy":{"friend_requests":"nobody"}}`), 200, "")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"friend_code": code}), 403, "requests_disabled")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": b.ID}), 403, "requests_disabled")
	// ... but nobody can still send out requests, and the reply is accepted
	f.expect(f.do("POST", "/friends/requests", b, map[string]string{"player_id": a.ID}), 201, "")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": b.ID}), 200, "")

	// not searchable -> indistinguishable from unknown
	f.expect(f.do("PUT", "/social/profile", c, `{"privacy":{"searchable":false}}`), 200, "")
	ccode := f.do("GET", "/social/profile", c, nil).Body["friend_code"].(string)
	hidden1 := f.expect(f.do("POST", "/friends/requests", a, map[string]string{"friend_code": ccode}), 404, "not_found")
	hidden2 := f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": c.ID}), 404, "not_found")
	unknown := f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": "plr_nope"}), 404, "not_found")
	for _, r := range []gametest.Resp{hidden1, hidden2} {
		if r.Body["error"].(map[string]any)["message"] != unknown.Body["error"].(map[string]any)["message"] {
			t.Fatalf("hidden response differs from unknown: %s vs %s", r.Raw, unknown.Raw)
		}
	}
}

func TestFriendLimits(t *testing.T) {
	f := setup(t, func(m *social.Module) { m.MaxFriends = 2; m.MaxPendingRequests = 2 })
	a, b, c, d := f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha")
	f.befriend(a, b)
	f.befriend(a, c)
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": d.ID}), 409, "friend_limit")
	// d can ask a (d has room), but a cannot accept because a is full
	f.expect(f.do("POST", "/friends/requests", d, map[string]string{"player_id": a.ID}), 201, "")
	f.expect(f.do("POST", "/friends/requests/"+d.ID+"/accept", a, nil), 409, "friend_limit")
	// pending request cap
	e1, e2, e3 := f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha")
	f.expect(f.do("POST", "/friends/requests", e1, map[string]string{"player_id": e2.ID}), 201, "")
	f.expect(f.do("POST", "/friends/requests", e1, map[string]string{"player_id": e3.ID}), 201, "")
	f.expect(f.do("POST", "/friends/requests", e1, map[string]string{"player_id": d.ID}), 409, "request_limit")
}

func TestBlocks(t *testing.T) {
	f := setup(t, nil)
	a, b, c := f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha")
	f.befriend(a, b)
	f.expect(f.do("POST", "/friends/requests", c, map[string]string{"player_id": a.ID}), 201, "")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": c.ID}), 200, "") // accepted: a-c friends
	f.expect(f.do("POST", "/friends/requests", b, map[string]string{"player_id": c.ID}), 201, "")

	f.expect(f.do("POST", "/blocks/"+a.ID, a, nil), 400, "")
	f.expect(f.do("POST", "/blocks/plr_nope", a, nil), 404, "")
	f.expect(f.do("POST", "/blocks/"+b.ID, a, nil), 200, "")
	f.expect(f.do("POST", "/blocks/"+b.ID, a, nil), 200, "") // idempotent
	if len(friendIDs(f.do("GET", "/friends", a, nil))) != 1 || len(friendIDs(f.do("GET", "/friends", b, nil))) != 0 {
		t.Fatal("block must remove friendship on both sides")
	}
	// pending requests between the pair are cleared
	f.expect(f.do("POST", "/blocks/"+c.ID, b, nil), 200, "") // b had a pending request to c
	if r := f.do("GET", "/friends/requests", c, nil); len(r.Body["incoming"].([]any)) != 0 {
		t.Fatalf("%s", r.Raw)
	}
	// both directions: uniform 404
	f.expect(f.do("POST", "/friends/requests", b, map[string]string{"player_id": a.ID}), 404, "not_found")
	f.expect(f.do("POST", "/friends/requests", a, map[string]string{"player_id": b.ID}), 404, "not_found")
	f.expect(f.do("POST", "/friends/requests", c, map[string]string{"player_id": b.ID}), 404, "not_found")
	bl := f.expect(f.do("GET", "/blocks", a, nil), 200, "")
	if len(bl.Body["blocks"].([]any)) != 1 {
		t.Fatalf("%s", bl.Raw)
	}
	// the blocked player's own view does not list the block
	if len(f.do("GET", "/blocks", b, nil).Body["blocks"].([]any)) != 1 { // b blocked c
		t.Fatal("b blocks list")
	}
	f.expect(f.do("DELETE", "/blocks/"+b.ID, a, nil), 200, "")
	f.expect(f.do("POST", "/friends/requests", b, map[string]string{"player_id": a.ID}), 201, "")
}

func balance(f *fixture, p gametest.Player, asset string) float64 {
	r := f.do("GET", "/wallet", p, nil)
	v, _ := r.Body["balances"].(map[string]any)[asset].(float64)
	return v
}

func TestInvites(t *testing.T) {
	f := setupPlatform(t, func(m *social.Module) {
		m.InviterGrant = map[string]int64{"gem": 10}
		m.InviteeGrant = map[string]int64{"coin": 5}
		m.MaxInvites = 2
		m.MaxAcceptsPerInviter = 3
	})
	inviter := f.e.Guest("alpha")
	code := f.expect(f.do("POST", "/invites", inviter, nil), 201, "").Body["code"].(string)
	f.expect(f.do("POST", "/invites", inviter, nil), 201, "")
	f.expect(f.do("POST", "/invites", inviter, nil), 409, "invite_limit")
	f.expect(f.do("POST", "/invites/"+code+"/accept", inviter, nil), 400, "self_invite")
	f.expect(f.do("POST", "/invites/ZZZZZZZZ/accept", f.e.Guest("alpha"), nil), 404, "")
	f.expect(f.do("POST", "/invites/bad!/accept", f.e.Guest("alpha"), nil), 404, "")

	n := f.e.Guest("alpha")
	f.bind(n) // only verified invitees are paid at accept time
	r1 := f.expect(f.do("POST", "/invites/"+code+"/accept", n, nil, "Idempotency-Key", "i1"), 200, "")
	r2 := f.expect(f.do("POST", "/invites/"+code+"/accept", n, nil, "Idempotency-Key", "i1"), 200, "")
	if r2.Header.Get("Idempotent-Replayed") != "true" || string(r1.Raw) != string(r2.Raw) {
		t.Fatal("replay expected")
	}
	f.expect(f.do("POST", "/invites/"+code+"/accept", n, nil), 409, "already_attributed")
	if balance(f, n, "coin") != 5 || balance(f, inviter, "gem") != 10 {
		t.Fatalf("grants: coin=%v gem=%v", balance(f, n, "coin"), balance(f, inviter, "gem"))
	}
	if r := f.do("GET", "/invites", inviter, nil); r.Body["invites"].([]any)[0].(map[string]any)["accepted"] != float64(1) {
		t.Fatalf("%s", r.Raw)
	}
	// codes are game scoped
	f.expect(f.e.Do("POST", "/v1/games/beta/invites/"+code+"/accept", f.e.Guest("beta").Token, nil), 404, "")

	// concurrent accepts by the same new player attribute and grant exactly once
	n2 := f.e.Guest("alpha")
	f.bind(n2)
	var wg sync.WaitGroup
	var ok, conflict atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch f.do("POST", "/invites/"+code+"/accept", n2, nil).Status {
			case 200:
				ok.Add(1)
			case 409:
				conflict.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || conflict.Load() != 7 {
		t.Fatalf("ok=%d conflict=%d", ok.Load(), conflict.Load())
	}
	if balance(f, n2, "coin") != 5 || balance(f, inviter, "gem") != 20 {
		t.Fatalf("grants after race: coin=%v gem=%v", balance(f, n2, "coin"), balance(f, inviter, "gem"))
	}
	// per-inviter cap
	f.expect(f.do("POST", "/invites/"+code+"/accept", f.e.Guest("alpha"), nil), 200, "")
	f.expect(f.do("POST", "/invites/"+code+"/accept", f.e.Guest("alpha"), nil), 409, "invite_full")
	if mm, err := game.Reconcile(t.Context(), f.e.App.DB.Read, "alpha"); err != nil || len(mm) != 0 {
		t.Fatalf("reconcile %v %v", mm, err)
	}
}

func TestInviteNewPlayerWindow(t *testing.T) {
	f := setup(t, nil)
	inviter := f.e.Guest("alpha")
	code := f.do("POST", "/invites", inviter, nil).Body["code"].(string)
	old := f.e.Guest("alpha")
	f.now.Store((25 * time.Hour).Milliseconds()) // the player is now 25h old
	f.expect(f.do("POST", "/invites/"+code+"/accept", old, nil), 403, "not_new_player")
}

func TestShares(t *testing.T) {
	f := setup(t, nil)
	a, b, c := f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha")
	f.expect(f.do("POST", "/shares", a, map[string]any{"channel": "Bad Channel", "payload": 1}), 400, "")
	id := f.expect(f.do("POST", "/shares", a, map[string]any{"channel": "wechat", "payload": map[string]any{"level": 3}}), 201, "").Body["share_id"].(string)
	f.expect(f.do("POST", "/shares/"+id+"/open", a, nil), 400, "own_share")
	f.expect(f.do("POST", "/shares/shr_unknown/open", b, nil), 404, "")
	r := f.expect(f.do("POST", "/shares/"+id+"/open", b, nil), 200, "")
	if r.Body["first_open"] != true || r.Body["channel"] != "wechat" {
		t.Fatalf("%s", r.Raw)
	}
	if f.expect(f.do("POST", "/shares/"+id+"/open", b, nil), 200, "").Body["first_open"] != false {
		t.Fatal("second open by the same player must not count")
	}
	f.expect(f.do("POST", "/shares/"+id+"/open", c, nil), 200, "")
	if g := f.expect(f.do("GET", "/shares/"+id, a, nil), 200, ""); g.Body["opens"] != float64(2) {
		t.Fatalf("%s", g.Raw)
	}
	f.expect(f.do("GET", "/shares/"+id, b, nil), 404, "") // only the sharer sees stats
	// not visible across games or to blocked players
	f.expect(f.e.Do("POST", "/v1/games/beta/shares/"+id+"/open", f.e.Guest("beta").Token, nil), 404, "")
	d := f.e.Guest("alpha")
	f.expect(f.do("POST", "/blocks/"+d.ID, a, nil), 200, "")
	f.expect(f.do("POST", "/shares/"+id+"/open", d, nil), 404, "")
}

func TestFriendBoard(t *testing.T) {
	f := setup(t, nil)
	a, b, c, d := f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha"), f.e.Guest("alpha")
	f.befriend(a, b)
	f.befriend(a, c)
	score := func(p gametest.Player, s int) {
		f.expect(f.do("POST", "/boards/score/scores", p, map[string]int{"score": s}), 200, "")
	}
	f.expect(f.do("PUT", "/social/profile", b, `{"display_name":"Bob"}`), 200, "")
	score(a, 100)
	score(b, 300)
	score(c, 200)
	score(d, 999) // not a friend
	r := f.expect(f.do("GET", "/friends/boards/score", a, nil), 200, "")
	es := r.Body["entries"].([]any)
	if len(es) != 3 {
		t.Fatalf("%s", r.Raw)
	}
	want := []string{b.ID, c.ID, a.ID}
	for i, e := range es {
		m := e.(map[string]any)
		if m["player_id"] != want[i] || m["rank"] != float64(i+1) || m["me"] != (want[i] == a.ID) {
			t.Fatalf("entry %d: %s", i, r.Raw)
		}
	}
	if es[0].(map[string]any)["display_name"] != "Bob" {
		t.Fatalf("%s", r.Raw)
	}
	// opt-out hides b from friends but b still sees self
	f.expect(f.do("PUT", "/social/profile", b, `{"privacy":{"show_on_friend_boards":false}}`), 200, "")
	if es := f.do("GET", "/friends/boards/score", a, nil).Body["entries"].([]any); len(es) != 2 {
		t.Fatalf("opt-out ignored: %v", es)
	}
	if es := f.do("GET", "/friends/boards/score", b, nil).Body["entries"].([]any); len(es) != 2 || es[0].(map[string]any)["player_id"] != b.ID {
		t.Fatalf("self missing: %v", es)
	}
	// removing a friend / blocking removes them from the board
	f.expect(f.do("POST", "/blocks/"+c.ID, a, nil), 200, "")
	if es := f.do("GET", "/friends/boards/score", a, nil).Body["entries"].([]any); len(es) != 1 {
		t.Fatalf("%v", es)
	}
	f.expect(f.do("GET", "/friends/boards/nope", a, nil), 404, "")
	f.expect(f.do("GET", "/friends/boards/score?season="+"xxxxxxxxxxxxxxxxxxxx", a, nil), 400, "")
}
