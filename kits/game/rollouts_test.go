package game_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pillar/gostack/kits/game/gametest"
)

const rolloutDoc = `{
  "client": {"a": 1, "b": 1},
  "rollouts": [
    {"id": "r_all", "enabled": true, "percent": 100, "platforms": ["wechat"], "min_client_version": "1.2.0", "max_client_version": "2", "client": {"b": 2}},
    {"id": "r_half", "enabled": true, "percent": 50, "client": {"a": 9}},
    {"id": "r_off", "enabled": false, "percent": 100, "client": {"a": 7}}
  ]
}`

func TestConfigRollouts(t *testing.T) {
	e := gametest.New(t, gametest.Options{Config: rolloutDoc})
	get := func(query, token string, hdr ...string) gametest.Resp {
		return e.Do("GET", "/v1/games/alpha/config"+query, token, nil, hdr...)
	}
	data := func(r gametest.Resp) map[string]any { return r.Body["data"].(map[string]any) }

	base := e.Expect(get("", ""), 200)
	if d := data(base); d["a"].(float64) != 1 || d["b"].(float64) != 1 || base.Body["rollouts"] != nil || base.Header.Get("ETag") != `"1"` {
		t.Fatalf("base %s %s", base.Raw, base.Header.Get("ETag"))
	}
	wc := e.Expect(get("?platform=wechat&client_version=1.2.0", ""), 200)
	if data(wc)["b"].(float64) != 2 || data(wc)["a"].(float64) != 1 || fmt.Sprint(wc.Body["rollouts"]) != "[r_all]" {
		t.Fatalf("wechat %s", wc.Raw)
	}
	etag := wc.Header.Get("ETag")
	if etag == `"1"` || !strings.HasPrefix(etag, `"1-`) {
		t.Fatalf("etag %s", etag)
	}
	e.Expect(get("?platform=wechat&client_version=1.2.0", "", "If-None-Match", etag), 304)
	// The base ETag no longer matches once a rollout applies, and vice versa.
	e.Expect(get("?platform=wechat&client_version=1.2.0", "", "If-None-Match", `"1"`), 200)
	e.Expect(get("", "", "If-None-Match", etag), 200)
	for _, q := range []string{"?platform=wechat&client_version=1.1.9", "?platform=wechat&client_version=2.0.1", "?platform=web&client_version=1.5", "?platform=wechat"} {
		if r := e.Expect(get(q, ""), 200); data(r)["b"].(float64) != 1 {
			t.Fatalf("%s matched: %s", q, r.Raw)
		}
	}
	if r := e.Expect(get("?platform=wechat&client_version=2", ""), 200); data(r)["b"].(float64) != 2 {
		t.Fatalf("max inclusive %s", r.Raw)
	}
	e.Expect(get("?client_version=1.x", ""), 400)
	e.Expect(get("?client_version=1..2", ""), 400)

	// Percentage bucketing is stable per key and roughly proportional.
	in := 0
	for i := 0; i < 200; i++ {
		q := fmt.Sprintf("?bucket_key=k%d", i)
		r1, r2 := e.Expect(get(q, ""), 200), e.Expect(get(q, ""), 200)
		if data(r1)["a"] != data(r2)["a"] {
			t.Fatal("unstable bucket")
		}
		if data(r1)["a"].(float64) == 9 {
			in++
		}
	}
	if in < 60 || in > 140 {
		t.Fatalf("50%% rollout matched %d/200", in)
	}
	// Authenticated players bucket by player_id (bucket_key is ignored); invalid tokens are ignored.
	p := e.Guest("alpha")
	byPlayer := data(e.Expect(get("?bucket_key=zzz", p.Token), 200))["a"]
	byKey := data(e.Expect(get("?bucket_key="+p.ID, ""), 200))["a"]
	if byPlayer != byKey {
		t.Fatalf("player bucket %v vs %v", byPlayer, byKey)
	}
	e.Expect(get("", "not-a-token"), 200)

	// Stop switch: republish with the rollout disabled.
	stopped := strings.Replace(rolloutDoc, `"id": "r_half", "enabled": true`, `"id": "r_half", "enabled": false`, 1)
	e.Expect(e.Do("PUT", "/v1/admin/games/alpha/config", e.AdminToken, stopped), 200)
	for i := 0; i < 50; i++ {
		if r := e.Expect(get(fmt.Sprintf("?bucket_key=k%d", i), ""), 200); data(r)["a"].(float64) != 1 || r.Header.Get("ETag") != `"2"` {
			t.Fatalf("stopped rollout still applied: %s", r.Raw)
		}
	}
}
