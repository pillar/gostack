package experiments_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/kits/game/experiments"
	"github.com/pillar/gostack/kits/game/gametest"
)

func vs(weights ...int) []experiments.Variant {
	out := make([]experiments.Variant, len(weights))
	for i, w := range weights {
		out[i] = experiments.Variant{ID: fmt.Sprintf("v%d", i), Weight: w, Params: json.RawMessage(`{}`)}
	}
	return out
}

func TestBucketStableAndPinned(t *testing.T) {
	a := experiments.Bucket("alpha", "layer:ui", "p1", 10000)
	for i := 0; i < 100; i++ {
		if experiments.Bucket("alpha", "layer:ui", "p1", 10000) != a {
			t.Fatal("not deterministic")
		}
	}
	// Pinned value: changing the hash would silently reshuffle every running experiment.
	if got := experiments.Bucket("alpha", "layer:ui", "p1", 10000); got != 3785 {
		t.Logf("pinned bucket is %d", got)
		t.Fatal("bucketing function changed")
	}
	// Separator prevents (game, salt) concatenation collisions.
	if experiments.Bucket("ab", "c", "p", 1<<40) == experiments.Bucket("a", "bc", "p", 1<<40) {
		t.Fatal("field boundary ambiguity")
	}
}

func TestDistributionMatchesWeights(t *testing.T) {
	const n = 10000
	variants := vs(50, 30, 20)
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		counts[experiments.PickVariant("alpha", "exp1", fmt.Sprintf("player-%d", i), variants).ID]++
	}
	for i, want := range []float64{0.5, 0.3, 0.2} {
		got := float64(counts[fmt.Sprintf("v%d", i)]) / n
		if math.Abs(got-want) > 0.02 {
			t.Fatalf("variant %d share %.3f want %.2f (%v)", i, got, want, counts)
		}
	}
	// Layer slots are uniform over 10 deciles.
	var dec [10]int
	for i := 0; i < n; i++ {
		dec[experiments.LayerSlot("alpha", "ui", fmt.Sprintf("player-%d", i))/1000]++
	}
	for d, c := range dec {
		if c < 850 || c > 1150 {
			t.Fatalf("decile %d has %d", d, c)
		}
	}
}

func TestAssignPureRules(t *testing.T) {
	e := experiments.Experiment{ID: "e", Layer: "l", Status: "running", Variants: vs(1, 1), Control: "v0", TrafficPercent: 100,
		Platforms: []string{"wechat"}, MinClientVersion: "1.2.0"}
	if _, ok := experiments.Assign("g", "p", e, "web", "1.2.0"); ok {
		t.Fatal("platform filter")
	}
	for _, v := range []string{"", "1.1.9", "1.1"} {
		if _, ok := experiments.Assign("g", "p", e, "wechat", v); ok {
			t.Fatalf("version %q should be excluded", v)
		}
	}
	for _, v := range []string{"1.2.0", "1.2", "1.10.0", "2"} {
		if _, ok := experiments.Assign("g", "p", e, "wechat", v); !ok {
			t.Fatalf("version %q should be included", v)
		}
	}
	e.Status = "draft"
	if _, ok := experiments.Assign("g", "p", e, "wechat", "2"); ok {
		t.Fatal("draft assigned")
	}
	e.Status = "stopped"
	a, ok := experiments.Assign("g", "p", e, "", "")
	if !ok || a.Variant != "v0" || !a.Stopped {
		t.Fatalf("stopped: %+v", a)
	}
}

type fx struct{ *gametest.Env }

func setup(t *testing.T) fx {
	return fx{gametest.New(t, gametest.Options{Routes: func(mux *http.ServeMux, a *app.App) {
		(&experiments.Module{Kit: a.Game, DB: a.DB}).Routes(mux)
	}})}
}

func admin(g, p string) string { return "/v1/admin/games/" + g + p }
func pub(g, p string) string   { return "/v1/games/" + g + p }

func exp(layer, status string, pct int, weights ...int) map[string]any {
	var variants []map[string]any
	for i, w := range weights {
		variants = append(variants, map[string]any{"id": fmt.Sprintf("v%d", i), "weight": w, "params": map[string]any{"i": i}})
	}
	return map[string]any{"layer": layer, "status": status, "variants": variants, "control": "v0", "traffic_percent": pct}
}

func (f fx) put(g, id string, body map[string]any, want int) gametest.Resp {
	f.T.Helper()
	return f.Expect(f.Do("PUT", admin(g, "/experiments/"+id), f.AdminToken, body), want)
}

func (f fx) assignments(g string, p gametest.Player, query string) map[string]any {
	f.T.Helper()
	return f.Expect(f.Do("GET", pub(g, "/experiments"+query), p.Token, nil), 200).Body["assignments"].(map[string]any)
}

func TestAdminValidationAndLifecycle(t *testing.T) {
	f := setup(t)
	p := f.Guest("alpha")
	good := exp("ui", "draft", 50, 1, 1)
	f.Expect(f.Do("PUT", admin("alpha", "/experiments/e1"), p.Token, good), 401)
	bad := func(mut func(m map[string]any)) {
		m := exp("ui", "draft", 50, 1, 1)
		mut(m)
		f.put("alpha", "e1", m, 400)
	}
	bad(func(m map[string]any) { m["control"] = "nope" })
	bad(func(m map[string]any) { delete(m, "traffic_percent") })
	bad(func(m map[string]any) { m["traffic_percent"] = 101 })
	bad(func(m map[string]any) { m["status"] = "paused" })
	bad(func(m map[string]any) { m["variants"] = []any{} })
	bad(func(m map[string]any) {
		m["variants"] = []any{map[string]any{"id": "a", "weight": 1}, map[string]any{"id": "a", "weight": 1}}
		m["control"] = "a"
	})
	bad(func(m map[string]any) { m["variants"] = []any{map[string]any{"id": "v0", "weight": 0}} })
	bad(func(m map[string]any) { m["min_client_version"] = "v1" })
	bad(func(m map[string]any) { m["layer"] = "Bad Layer" })
	bad(func(m map[string]any) { m["bogus"] = 1 })

	f.put("alpha", "e1", good, 200)
	// Drafts are invisible to players; unknown experiment is 404.
	if len(f.assignments("alpha", p, "")) != 0 {
		t.Fatal("draft visible")
	}
	f.Expect(f.Do("GET", admin("alpha", "/experiments/zzz"), f.AdminToken, nil), 404)
	// Draft may be edited, then started.
	good["traffic_percent"] = 100
	f.put("alpha", "e1", good, 200)
	good["status"] = "running"
	r := f.put("alpha", "e1", good, 200)
	if r.Body["slice_start"] != 0.0 || r.Body["slice_end"] != 10000.0 {
		t.Fatalf("%v", r.Body)
	}
	if as := f.assignments("alpha", p, ""); len(as) != 1 {
		t.Fatalf("%v", as)
	}
	// Running: variants/layer frozen, traffic editable, no way back to draft.
	frozen := exp("ui", "running", 100, 1, 3)
	if c := f.put("alpha", "e1", frozen, 409).Code(); c != "immutable_while_running" {
		t.Fatal(c)
	}
	if c := f.put("alpha", "e1", exp("other", "running", 100, 1, 1), 409).Code(); c != "immutable_while_running" {
		t.Fatal(c)
	}
	if c := f.put("alpha", "e1", exp("ui", "draft", 100, 1, 1), 409).Code(); c != "invalid_transition" {
		t.Fatal(c)
	}
	f.put("alpha", "e1", exp("ui", "running", 40, 1, 1), 200)
	f.Expect(f.Do("DELETE", admin("alpha", "/experiments/e1"), f.AdminToken, nil), 409)
	// Stopped is terminal.
	f.Expect(f.Do("POST", admin("alpha", "/experiments/e1/stop"), f.AdminToken, nil), 200)
	if c := f.put("alpha", "e1", exp("ui", "running", 40, 1, 1), 409).Code(); c != "experiment_stopped" {
		t.Fatal(c)
	}
	f.Expect(f.Do("DELETE", admin("alpha", "/experiments/e1"), f.AdminToken, nil), 204)
	f.Expect(f.Do("DELETE", admin("alpha", "/experiments/e1"), f.AdminToken, nil), 404)
}

func TestLayerExclusivityAndSlices(t *testing.T) {
	f := setup(t)
	f.put("alpha", "a", exp("ui", "running", 30, 1, 1), 200)
	f.put("alpha", "b", exp("ui", "running", 30, 1, 1), 200)
	f.put("alpha", "c", exp("ui", "running", 40, 1, 1), 200)
	if c := f.put("alpha", "d", exp("ui", "running", 1, 1, 1), 409).Code(); c != "layer_full" {
		t.Fatal(c)
	}
	f.put("alpha", "other", exp("shop", "running", 100, 1, 1), 200) // layers are independent
	got := f.Expect(f.Do("GET", admin("alpha", "/experiments"), f.AdminToken, nil), 200).Body["experiments"].([]any)
	starts := map[string]float64{}
	for _, x := range got {
		m := x.(map[string]any)
		starts[m["id"].(string)] = m["slice_start"].(float64)
	}
	if starts["a"] != 0 || starts["b"] != 3000 || starts["c"] != 6000 {
		t.Fatalf("%v", starts)
	}

	// Pure check over 10k synthetic players: at most one experiment per layer, shares match traffic.
	list := func() []experiments.Experiment {
		out := []experiments.Experiment{}
		for _, x := range got {
			b, _ := json.Marshal(x)
			var e experiments.Experiment
			json.Unmarshal(b, &e)
			var m map[string]any
			json.Unmarshal(b, &m)
			e.SliceStart = int(m["slice_start"].(float64))
			out = append(out, e)
		}
		return out
	}()
	perExp := map[string]int{}
	for i := 0; i < 10000; i++ {
		player := fmt.Sprintf("p%d", i)
		perLayer := map[string]int{}
		for _, e := range list {
			if a, ok := experiments.Assign("alpha", player, e, "", ""); ok {
				perLayer[a.Layer]++
				perExp[e.ID]++
			}
		}
		if perLayer["ui"] > 1 {
			t.Fatalf("player %s in %d ui experiments", player, perLayer["ui"])
		}
		if perLayer["ui"] != 1 {
			t.Fatalf("100%% of the layer is allocated yet %s has none", player)
		}
	}
	for id, want := range map[string]float64{"a": 0.3, "b": 0.3, "c": 0.4, "other": 1} {
		if g := float64(perExp[id]) / 10000; math.Abs(g-want) > 0.02 {
			t.Fatalf("%s share %.3f want %.2f", id, g, want)
		}
	}

	// Stopping b frees its slice for a new experiment and never moves a or c.
	before := map[string]string{}
	players := make([]gametest.Player, 12)
	for i := range players {
		players[i] = f.Guest("alpha")
		for id, a := range f.assignments("alpha", players[i], "") {
			if id == "a" || id == "c" {
				before[players[i].ID+id] = a.(map[string]any)["variant"].(string)
			}
		}
	}
	f.Expect(f.Do("POST", admin("alpha", "/experiments/b/stop"), f.AdminToken, nil), 200)
	f.put("alpha", "d", exp("ui", "running", 30, 1, 1), 200)
	if s := f.Expect(f.Do("GET", admin("alpha", "/experiments/d"), f.AdminToken, nil), 200).Body["slice_start"]; s != 3000.0 {
		t.Fatalf("d slice %v", s)
	}
	for _, p := range players {
		as := f.assignments("alpha", p, "")
		for _, id := range []string{"a", "c"} {
			if a, ok := as[id]; ok != (before[p.ID+id] != "") || (ok && a.(map[string]any)["variant"] != before[p.ID+id]) {
				t.Fatalf("player moved in %s", id)
			}
		}
	}
	// Growing a running experiment is refused when it would overlap its neighbour (d now sits at 3000);
	// shrinking is fine and keeps the slice start.
	if c := f.put("alpha", "a", exp("ui", "running", 31, 1, 1), 409).Code(); c != "layer_full" {
		t.Fatal(c)
	}
	f.put("alpha", "a", exp("ui", "running", 10, 1, 1), 200)
}

func TestKillSwitchIdempotentAndExposures(t *testing.T) {
	f := setup(t)
	f.put("alpha", "e1", exp("ui", "running", 100, 1, 1, 1), 200)
	players := make([]gametest.Player, 30)
	variants := map[string]int{}
	for i := range players {
		players[i] = f.Guest("alpha")
		a := f.assignments("alpha", players[i], "")["e1"].(map[string]any)
		variants[a["variant"].(string)]++
		if _, has := a["stopped"]; has {
			t.Fatal("stopped flag set while running")
		}
		// Stable across calls.
		if again := f.assignments("alpha", players[i], "")["e1"].(map[string]any); again["variant"] != a["variant"] {
			t.Fatal("assignment changed between calls")
		}
		f.Expect(f.Do("POST", pub("alpha", "/experiments/exposures"), players[i].Token, map[string]any{"experiment_ids": []string{"e1", "e1", "ghost"}}), 204)
	}
	if len(variants) < 2 {
		t.Fatalf("no spread across variants: %v", variants)
	}
	// Repeated exposures are deduplicated: one row per player.
	f.Expect(f.Do("POST", pub("alpha", "/experiments/exposures"), players[0].Token, map[string]any{"experiment_ids": []string{"e1"}}), 204)
	res := f.Expect(f.Do("GET", admin("alpha", "/experiments/e1/results"), f.AdminToken, nil), 200).Body
	if res["total_exposures"] != 30.0 {
		t.Fatalf("%v", res)
	}
	sum := 0.0
	for _, v := range res["variants"].([]any) {
		m := v.(map[string]any)
		sum += m["exposures"].(float64)
		if m["exposures"] != float64(variants[m["variant"].(string)]) {
			t.Fatalf("variant counts %v vs %v", m, variants)
		}
	}
	if sum != 30 {
		t.Fatal(sum)
	}
	f.Expect(f.Do("POST", pub("alpha", "/experiments/exposures"), players[0].Token, map[string]any{"experiment_ids": []string{}}), 400)
	f.Expect(f.Do("POST", pub("alpha", "/experiments/exposures"), "", map[string]any{"experiment_ids": []string{"e1"}}), 401)

	// Kill switch: everyone gets control with its params at once; repeated stops are harmless.
	for i := 0; i < 3; i++ {
		r := f.Expect(f.Do("POST", admin("alpha", "/experiments/e1/stop"), f.AdminToken, nil), 200)
		if r.Body["status"] != "stopped" {
			t.Fatalf("%v", r.Body)
		}
	}
	for _, p := range players {
		a := f.assignments("alpha", p, "")["e1"].(map[string]any)
		if a["variant"] != "v0" || a["stopped"] != true || a["params"].(map[string]any)["i"] != 0.0 {
			t.Fatalf("not control after stop: %v", a)
		}
	}
	// Exposures after stop are not recorded; results are kept.
	late := f.Guest("alpha")
	f.Expect(f.Do("POST", pub("alpha", "/experiments/exposures"), late.Token, map[string]any{"experiment_ids": []string{"e1"}}), 204)
	if f.Expect(f.Do("GET", admin("alpha", "/experiments/e1/results"), f.AdminToken, nil), 200).Body["total_exposures"] != 30.0 {
		t.Fatal("exposure recorded after stop")
	}
	f.Expect(f.Do("POST", admin("alpha", "/experiments/ghost/stop"), f.AdminToken, nil), 404)
	// Stopping via PUT is the same kill switch.
	f.put("alpha", "e2", exp("shop", "running", 100, 1, 1), 200)
	f.put("alpha", "e2", exp("shop", "stopped", 100, 1, 1), 200)
	f.put("alpha", "e2", exp("shop", "stopped", 100, 1, 1), 409) // terminal
}

func TestFiltersAndCrossGameIsolation(t *testing.T) {
	f := setup(t)
	body := exp("ui", "running", 100, 1, 1)
	body["platforms"] = []string{"wechat"}
	body["min_client_version"] = "1.2.0"
	f.put("alpha", "wx", body, 200)
	a, b := f.Guest("alpha"), f.Guest("beta")

	if len(f.assignments("alpha", a, "")) != 0 || len(f.assignments("alpha", a, "?platform=web&client_version=2.0.0")) != 0 ||
		len(f.assignments("alpha", a, "?platform=wechat&client_version=1.1.0")) != 0 {
		t.Fatal("ineligible client assigned")
	}
	if len(f.assignments("alpha", a, "?platform=wechat&client_version=1.2.1")) != 1 {
		t.Fatal("eligible client not assigned")
	}
	// Exposure respects eligibility too.
	f.Expect(f.Do("POST", pub("alpha", "/experiments/exposures?platform=web"), a.Token, map[string]any{"experiment_ids": []string{"wx"}}), 204)
	if f.Expect(f.Do("GET", admin("alpha", "/experiments/wx/results"), f.AdminToken, nil), 200).Body["total_exposures"] != 0.0 {
		t.Fatal("ineligible exposure recorded")
	}
	f.Expect(f.Do("POST", pub("alpha", "/experiments/exposures?platform=wechat&client_version=1.3"), a.Token, map[string]any{"experiment_ids": []string{"wx"}}), 204)
	if f.Expect(f.Do("GET", admin("alpha", "/experiments/wx/results"), f.AdminToken, nil), 200).Body["total_exposures"] != 1.0 {
		t.Fatal("exposure missing")
	}
	// beta does not see alpha's experiment, cannot expose to it, and has its own namespace.
	if len(f.assignments("beta", b, "?platform=wechat&client_version=9")) != 0 {
		t.Fatal("cross-game assignment")
	}
	f.Expect(f.Do("POST", pub("beta", "/experiments/exposures?platform=wechat&client_version=9"), b.Token, map[string]any{"experiment_ids": []string{"wx"}}), 204)
	f.Expect(f.Do("GET", admin("beta", "/experiments/wx/results"), f.AdminToken, nil), 404)
	f.Expect(f.Do("GET", pub("alpha", "/experiments"), b.Token, nil), 403) // session of another game
	body2 := exp("ui", "running", 100, 1, 1)
	f.put("beta", "wx", body2, 200) // same id, independent layer allocation
	if len(f.assignments("beta", b, "")) != 1 {
		t.Fatal("beta experiment missing")
	}
	// Same player id buckets differently per game (salted by game).
	same := 0
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("p%d", i)
		if experiments.LayerSlot("alpha", "ui", id) == experiments.LayerSlot("beta", "ui", id) {
			same++
		}
	}
	if same > 10 {
		t.Fatalf("slots correlate across games: %d/1000 equal", same)
	}
}
