package match_test

import (
	"context"
	"testing"

	"github.com/pillar/gostack/core/auth"
)

// The room credentials are bound to the claiming key: another battle key of
// the same tenant cannot drive the room even with the right token and epoch.
func TestRoomBoundToClaimingKey(t *testing.T) {
	f := setup(t)
	ps, _, roomID := f.pair()
	_, second, err := auth.APIKeys{DB: f.e.App.DB}.Create(context.Background(), "default", "battle2", []string{"battle"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := f.expect(f.claim(f.battle, map[string]any{"server_id": "bs1"}), 200, "")
	tok := r.Body["token"]
	f.expect(f.room(roomID, "heartbeat", second, map[string]any{"token": tok}), 409, "stale_token")
	f.expect(f.e.Do("POST", "/v1/match/rooms/"+roomID+"/members/"+ps[0].ID+"/state", second, map[string]any{"token": tok, "state": "left"}), 409, "stale_token")
	f.expect(f.room(roomID, "result", second, map[string]any{"token": tok, "results": []map[string]any{{"player_id": ps[0].ID, "grants": map[string]int{"coin": 1000}}}}), 409, "stale_token")
	if b := f.e.Do("GET", "/v1/games/alpha/wallet", ps[0].Token, nil).Body["balances"].(map[string]any); len(b) != 0 {
		t.Fatalf("foreign key settled the room: %v", b)
	}
	// The claimer still holds the room.
	f.expect(f.room(roomID, "heartbeat", f.battle, map[string]any{"token": tok}), 200, "")
}

// Tokens are random, not a guessable counter; the epoch is returned separately
// and keeps growing per game.
func TestRoomTokenIsRandomAndEpochIsReturned(t *testing.T) {
	f := setup(t)
	_, _, r1 := f.pair()
	_, _, r2 := f.pair()
	a := f.expect(f.claim(f.battle, map[string]any{"server_id": "bs1"}), 200, "")
	b := f.expect(f.claim(f.battle, map[string]any{"server_id": "bs1"}), 200, "")
	ta, ok1 := a.Body["token"].(string)
	tb, ok2 := b.Body["token"].(string)
	if !ok1 || !ok2 || len(ta) < 40 || ta == tb {
		t.Fatalf("tokens %v %v", a.Body["token"], b.Body["token"])
	}
	ea, _ := a.Body["epoch"].(float64)
	eb, _ := b.Body["epoch"].(float64)
	if ea < 1 || eb <= ea {
		t.Fatalf("epochs %v %v", ea, eb)
	}
	// The epoch alone (the old token) is not accepted, and a token is room scoped.
	f.expect(f.room(r1, "heartbeat", f.battle, map[string]any{"token": ea}), 400, "")
	f.expect(f.room(r2, "heartbeat", f.battle, map[string]any{"token": ta}), 409, "stale_token")
	var hash string
	f.e.App.DB.Read.QueryRow(`SELECT token_hash FROM match_rooms WHERE room_id=?`, r1).Scan(&hash)
	if hash == "" || hash == ta {
		t.Fatalf("token stored in clear or missing: %q", hash)
	}
}
