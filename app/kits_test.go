package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/kits/agent"
	"github.com/pillar/gostack/modules/jobs"
)

// kits: [agent, web] wires both kits: hook workflows are registered, routes
// are mounted, and the worker executes agent runs and web jobs.
func TestAgentAndWebKitsWired(t *testing.T) {
	c := testConfig(t)
	c.Kits = []string{"agent", "web"}
	c.Billing.Enabled = true
	a := newTestApp(t, c)
	if a.Agent == nil || a.Web == nil || a.Game != nil {
		t.Fatalf("kits not built: agent=%v web=%v game=%v", a.Agent != nil, a.Web != nil, a.Game != nil)
	}
	hooks := Hooks{Workflows: map[string]agent.Workflow{"echo": {Func: func(ctx context.Context, r *agent.Run) error {
		out, err := r.Step(ctx, "echo", func(context.Context) (agent.Output, error) { return agent.Output{Data: r.Input()}, nil })
		if err != nil {
			return err
		}
		return r.SetOutput(json.RawMessage(out))
	}}}}
	srv := httptest.NewServer(a.Handler(hooks))
	defer srv.Close()
	ctx := context.Background()
	_, key, err := (auth.APIKeys{DB: a.DB}).Create(ctx, "default", "k", []string{"agent", "web"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, body string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(res.Body).Decode(&m)
		if res.StatusCode != 201 && res.StatusCode != 200 {
			t.Fatalf("%s: %d %v", path, res.StatusCode, m)
		}
		return m
	}
	run := post("/v1/agent/runs", `{"workflow":"echo","input":{"x":1}}`)
	post("/v1/web/sites", `{"id":"s","name":"S","base_url":"https://s.example.com"}`)
	page := post("/v1/web/sites/s/pages", `{"slug":"later","title":"Later"}`)
	post("/v1/web/pages/"+page["id"].(string)+"/publish", `{"at":`+jsonInt(time.Now().Add(300*time.Millisecond).UnixMilli())+`}`)

	wctx, cancel := context.WithCancel(ctx)
	w := &jobs.Worker{Queue: a.Jobs, Handlers: a.kitJobHandlers(), Workers: 1, Poll: 20 * time.Millisecond, Lease: c.Queue.Lease, Log: a.Log}
	done := make(chan struct{})
	go func() { w.Run(wctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := a.Agent.Get(ctx, "default", run["id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		p, err := a.Web.GetPage(ctx, "default", page["id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == agent.StatusSucceeded && p.Status == "published" {
			if string(r.Output) != `{"x":1}` {
				t.Fatalf("output %s", r.Output)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("run or scheduled publish did not complete")
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }
