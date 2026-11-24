package game_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game/gametest"
)

func putSave(e *gametest.Env, p gametest.Player, expected int, payload any, extra map[string]any, hdr ...string) gametest.Resp {
	body := map[string]any{"expected_revision": expected, "schema_version": 2, "payload": payload}
	for k, v := range extra {
		body[k] = v
	}
	return e.Do("PUT", "/v1/games/alpha/saves/main", p.Token, body, hdr...)
}

func TestSaveRevisionsRestoreDelete(t *testing.T) {
	e := gametest.New(t, gametest.Options{Config: `{"saves": {"min_schema_version": 2}}`})
	p, other := e.Guest("alpha"), e.Guest("alpha")
	for i := 0; i < 3; i++ {
		e.Expect(putSave(e, p, i, map[string]int{"lvl": i + 1}, nil), 200)
	}
	// Schema compatibility.
	if r := e.Do("PUT", "/v1/games/alpha/saves/main", p.Token, map[string]any{"expected_revision": 3, "schema_version": 1, "payload": 1}); r.Code() != "schema_outdated" ||
		r.Body["error"].(map[string]any)["details"].(map[string]any)["min_schema_version"].(float64) != 2 {
		t.Fatalf("schema %s", r.Raw)
	}
	// Revisions, newest first, bounded.
	r := e.Expect(e.Do("GET", "/v1/games/alpha/saves/main/revisions", p.Token, nil), 200)
	revs := r.Body["revisions"].([]any)
	if len(revs) != 3 || revs[0].(map[string]any)["revision"].(float64) != 3 || r.Body["current_revision"].(float64) != 3 {
		t.Fatalf("revisions %s", r.Raw)
	}
	if r := e.Expect(e.Do("GET", "/v1/games/alpha/saves/main/revisions?limit=1", p.Token, nil), 200); len(r.Body["revisions"].([]any)) != 1 {
		t.Fatalf("limit %s", r.Raw)
	}
	e.Expect(e.Do("GET", "/v1/games/alpha/saves/main/revisions?limit=0", p.Token, nil), 400)
	e.Expect(e.Do("GET", "/v1/games/alpha/saves/main/revisions", other.Token, nil), 404) // scoped by session
	e.Expect(e.Do("GET", "/v1/games/alpha/saves/main/revisions", "", nil), 401)

	// Restore revision 1 as revision 4 (conditional + idempotent).
	restore := func(rev, expected int, key string) gametest.Resp {
		return e.Do("POST", "/v1/games/alpha/saves/main/restore", p.Token, map[string]int{"revision": rev, "expected_revision": expected}, "Idempotency-Key", key)
	}
	if r := restore(1, 2, "r0"); r.Code() != "revision_conflict" {
		t.Fatalf("stale restore %s", r.Raw)
	}
	e.Expect(restore(9, 3, "r9"), 404)
	r = e.Expect(restore(1, 3, "r1"), 200)
	if r.Body["revision"].(float64) != 4 || r.Body["payload"].(map[string]any)["lvl"].(float64) != 1 {
		t.Fatalf("restore %s", r.Raw)
	}
	rr := e.Expect(restore(1, 3, "r1"), 200)
	if rr.Header.Get("Idempotent-Replayed") != "true" || rr.Body["revision"].(float64) != 4 {
		t.Fatalf("restore replay %s", rr.Raw)
	}
	g := e.Expect(e.Do("GET", "/v1/games/alpha/saves/main", p.Token, nil), 200)
	if g.Body["revision"].(float64) != 4 || g.Body["checksum"] != r.Body["checksum"] {
		t.Fatalf("after restore %s", g.Raw)
	}

	// Delete: conditional, removes history, revision counter restarts.
	e.Expect(e.Do("DELETE", "/v1/games/alpha/saves/main", p.Token, nil), 400)
	if r := e.Do("DELETE", "/v1/games/alpha/saves/main?expected_revision=3", p.Token, nil); r.Code() != "revision_conflict" {
		t.Fatalf("stale delete %s", r.Raw)
	}
	e.Expect(e.Do("DELETE", "/v1/games/alpha/saves/main?expected_revision=4", other.Token, nil), 404)
	e.Expect(e.Do("DELETE", "/v1/games/alpha/saves/main?expected_revision=4", p.Token, nil, "Idempotency-Key", "d1"), 200)
	if r := e.Do("DELETE", "/v1/games/alpha/saves/main?expected_revision=4", p.Token, nil, "Idempotency-Key", "d1"); r.Status != 200 || r.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("delete replay %s", r.Raw)
	}
	e.Expect(e.Do("GET", "/v1/games/alpha/saves/main", p.Token, nil), 404)
	e.Expect(e.Do("GET", "/v1/games/alpha/saves/main/revisions", p.Token, nil), 404)
	if r := e.Expect(putSave(e, p, 0, map[string]int{"lvl": 9}, nil), 200); r.Body["revision"].(float64) != 1 {
		t.Fatalf("recreate %s", r.Raw)
	}
	// Without a merge hook, merge:true is an ordinary conflict.
	if r := putSave(e, p, 0, 1, map[string]any{"merge": true}); r.Code() != "revision_conflict" {
		t.Fatalf("merge without hook %s", r.Raw)
	}
}

func TestSaveMergeHook(t *testing.T) {
	e := gametest.New(t, gametest.Options{
		Mutate: func(c *config.Config) { c.Game.SaveHistory = 2 },
		Routes: func(_ *http.ServeMux, a *app.App) {
			// Union of top-level keys: server wins over base, client wins over server.
			a.Game.SaveMerge = func(_ context.Context, slot string, base, server, client json.RawMessage) (json.RawMessage, error) {
				out := map[string]any{}
				for _, b := range []json.RawMessage{base, server, client} {
					if len(b) > 0 {
						var m map[string]any
						if err := json.Unmarshal(b, &m); err != nil {
							return nil, err
						}
						for k, v := range m {
							out[k] = v
						}
					}
				}
				out["merged_slot"] = slot
				return json.Marshal(out)
			}
		},
	})
	p := e.Guest("alpha")
	e.Expect(putSave(e, p, 0, map[string]int{"a": 1}, nil), 200)
	e.Expect(putSave(e, p, 1, map[string]int{"a": 1, "b": 2}, nil), 200)
	// Stale write without merge → 409.
	if r := putSave(e, p, 1, map[string]int{"a": 1, "c": 3}, nil); r.Code() != "revision_conflict" {
		t.Fatalf("conflict %s", r.Raw)
	}
	r := e.Expect(putSave(e, p, 1, map[string]int{"a": 1, "c": 3}, map[string]any{"merge": true}), 200)
	pl := r.Body["payload"].(map[string]any)
	if r.Body["merged"] != true || r.Body["revision"].(float64) != 3 || pl["b"].(float64) != 2 || pl["c"].(float64) != 3 || pl["merged_slot"] != "main" {
		t.Fatalf("merge %s", r.Raw)
	}
	g := e.Expect(e.Do("GET", "/v1/games/alpha/saves/main", p.Token, nil), 200)
	if g.Body["checksum"] != r.Body["checksum"] {
		t.Fatalf("checksum %s vs %s", g.Raw, r.Raw)
	}
	// History keeps 2 revisions (2,3): base revision 1 is gone → 409 merge_base_unavailable.
	if r := putSave(e, p, 1, map[string]int{"d": 4}, map[string]any{"merge": true}); r.Code() != "merge_base_unavailable" {
		t.Fatalf("no base %s", r.Raw)
	}
	if r := e.Expect(e.Do("GET", "/v1/games/alpha/saves/main/revisions", p.Token, nil), 200); len(r.Body["revisions"].([]any)) != 2 {
		t.Fatalf("history %s", r.Raw)
	}
	// expected_revision 0 merges without a base.
	if r := e.Expect(putSave(e, p, 0, map[string]int{"e": 5}, map[string]any{"merge": true}), 200); r.Body["revision"].(float64) != 4 {
		t.Fatalf("merge from 0 %s", r.Raw)
	}
}
