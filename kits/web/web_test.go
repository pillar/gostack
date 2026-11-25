package web_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/web"
	"github.com/pillar/gostack/modules/billing"
	"github.com/pillar/gostack/modules/jobs"
	"github.com/pillar/gostack/modules/webauth"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	k   *web.Kit
	q   *jobs.Queue
	b   *billing.Service
	clk *clock
	srv *httptest.Server
}

func newEnv(t *testing.T, withSessions bool) *env {
	t.Helper()
	o := sqlite.Options{Path: filepath.Join(t.TempDir(), "app.db"), ReadConnections: 4, Synchronous: "FULL",
		BusyTimeout: time.Second, WALAutocheckpoint: 1000, WriteQueue: 256}
	db := pgtest.Open(t, o)
	clk := &clock{t: time.Now().Truncate(time.Second)}
	q := &jobs.Queue{DB: db, Now: clk.Now}
	b := &billing.Service{DB: db, Now: clk.Now}
	k := &web.Kit{DB: db, Jobs: q, Keys: auth.APIKeys{DB: db}, Billing: b, Cfg: config.Defaults().Web, Now: clk.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if withSessions {
		cfg := config.Defaults().Auth.Web
		cfg.Enabled = true
		k.WebAuth = &webauth.Module{DB: db, Cfg: cfg}
		k.WebAuth.Init()
	}
	mux := http.NewServeMux()
	k.Routes(mux)
	if k.WebAuth != nil {
		k.WebAuth.Routes(mux)
	}
	e := &env{k: k, q: q, b: b, clk: clk, srv: httptest.NewServer(mux)}
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) key(t *testing.T, tenant string, scopes ...string) string {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []string{"web"}
	}
	_, s, err := e.k.Keys.Create(context.Background(), tenant, "k", scopes, 0)
	must(t, err)
	return s
}

type resp struct {
	status int
	hdr    http.Header
	body   []byte
	m      map[string]any
}

func (e *env) do(t *testing.T, c *http.Client, method, path, key, body string, hdr ...string) resp {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	must(t, err)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, hdr: res.Header, body: b}
	_ = json.Unmarshal(b, &r.m)
	return r
}

func (r resp) code() string {
	if e, ok := r.m["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

// runJobs processes runnable web jobs; it returns the handler errors.
func (e *env) runJobs(t *testing.T) []error {
	t.Helper()
	var errs []error
	h := e.k.JobHandlers()
	for i := 0; i < 50; i++ {
		c, err := e.q.Claim(context.Background(), []string{web.JobPublish, web.JobWebhook}, 30*time.Second)
		must(t, err)
		if c == nil {
			return errs
		}
		if err := h[c.Type](context.Background(), c); err != nil {
			errs = append(errs, err)
			must(t, e.q.Nack(context.Background(), c, err))
		} else {
			must(t, e.q.Ack(context.Background(), c))
		}
	}
	t.Fatal("jobs did not drain")
	return nil
}

func TestContentLifecycleAndPublicCaching(t *testing.T) {
	e := newEnv(t, false)
	ka := e.key(t, "ta")
	if r := e.do(t, nil, "POST", "/v1/web/sites", ka, `{"id":"blog","name":"Blog","base_url":"https://blog.example.com/"}`); r.status != 201 || r.m["base_url"] != "https://blog.example.com" {
		t.Fatalf("site %d %s", r.status, r.body)
	}
	if r := e.do(t, nil, "POST", "/v1/web/sites", e.key(t, "tb"), `{"id":"blog","name":"B","base_url":"https://b.example.com"}`); r.status != 409 {
		t.Fatalf("duplicate site id %d", r.status)
	}
	r := e.do(t, nil, "POST", "/v1/web/sites/blog/pages", ka, `{"kind":"post","slug":"news/hello","title":"Hello","body":"v1","seo":{"description":"first"}}`)
	if r.status != 201 || r.m["status"] != "draft" || r.m["draft_revision"].(float64) != 1 {
		t.Fatalf("create page %d %s", r.status, r.body)
	}
	id := r.m["id"].(string)
	if r := e.do(t, nil, "POST", "/v1/web/sites/blog/pages", ka, `{"slug":"news/hello","title":"Dup"}`); r.code() != "slug_taken" {
		t.Fatalf("slug uniqueness %d %s", r.status, r.body)
	}
	if r := e.do(t, nil, "POST", "/v1/web/sites/blog/pages", ka, `{"slug":"Bad Slug","title":"x"}`); r.status != 400 {
		t.Fatalf("bad slug %d", r.status)
	}
	if r := e.do(t, nil, "GET", "/v1/web/public/blog/pages/news/hello", "", ""); r.status != 404 {
		t.Fatalf("draft is public: %d", r.status)
	}
	if r := e.do(t, nil, "POST", "/v1/web/pages/"+id+"/publish", ka, `{}`); r.status != 200 || r.m["status"] != "published" {
		t.Fatalf("publish %d %s", r.status, r.body)
	}
	pub := e.do(t, nil, "GET", "/v1/web/public/blog/pages/news/hello", "", "")
	etag := pub.hdr.Get("ETag")
	seo, _ := pub.m["seo"].(map[string]any)
	if pub.status != 200 || pub.m["body"] != "v1" || etag == "" || !strings.Contains(pub.hdr.Get("Cache-Control"), "max-age=60") ||
		seo["title"] != "Hello" || seo["canonical_url"] != "https://blog.example.com/news/hello" || seo["description"] != "first" {
		t.Fatalf("public %d %v %s", pub.status, pub.hdr, pub.body)
	}
	if r := e.do(t, nil, "GET", "/v1/web/public/blog/pages/news/hello", "", "", "If-None-Match", etag); r.status != 304 || len(r.body) != 0 {
		t.Fatalf("conditional GET %d", r.status)
	}
	// Editing creates revision 2 but the live page keeps revision 1.
	if r := e.do(t, nil, "PUT", "/v1/web/pages/"+id, ka, `{"slug":"news/hello","title":"Hello","body":"v2","expected_version":1}`); r.code() != "revision_conflict" {
		t.Fatalf("stale version %d %s", r.status, r.body)
	}
	r = e.do(t, nil, "PUT", "/v1/web/pages/"+id, ka, `{"slug":"news/hello","title":"Hello","body":"v2","expected_version":2}`)
	if r.status != 200 || r.m["draft_revision"].(float64) != 2 || r.m["published_revision"].(float64) != 1 {
		t.Fatalf("update %d %s", r.status, r.body)
	}
	if r := e.do(t, nil, "GET", "/v1/web/public/blog/pages/news/hello", "", ""); r.m["body"] != "v1" {
		t.Fatalf("unpublished edit leaked: %s", r.body)
	}
	version := r.m["version"].(float64)
	if r := e.do(t, nil, "POST", "/v1/web/pages/"+id+"/publish", ka, `{"revision":1}`); r.m["version"].(float64) != version {
		t.Fatalf("republishing the live revision must be a no-op: %s", r.body)
	}
	e.do(t, nil, "POST", "/v1/web/pages/"+id+"/publish", ka, `{}`)
	if r := e.do(t, nil, "GET", "/v1/web/public/blog/pages/news/hello", "", "", "If-None-Match", etag); r.status != 200 || r.m["body"] != "v2" {
		t.Fatalf("new revision with old etag %d %s", r.status, r.body)
	}
	if r := e.do(t, nil, "GET", "/v1/web/pages/"+id+"/revisions", ka, ""); len(r.m["revisions"].([]any)) != 2 {
		t.Fatalf("revisions %s", r.body)
	}
	// Invalid transitions.
	if r := e.do(t, nil, "POST", "/v1/web/pages/"+id+"/unschedule", ka, ``); r.code() != "invalid_transition" {
		t.Fatalf("unschedule published %d %s", r.status, r.body)
	}
	future := strconv.FormatInt(e.clk.Now().Add(time.Hour).UnixMilli(), 10)
	if r := e.do(t, nil, "POST", "/v1/web/pages/"+id+"/publish", ka, `{"at":`+future+`}`); r.code() != "invalid_transition" {
		t.Fatalf("schedule published %d %s", r.status, r.body)
	}
	// Archive takes it offline (idempotently); publishing brings it back.
	for i := 0; i < 2; i++ {
		if r := e.do(t, nil, "POST", "/v1/web/pages/"+id+"/archive", ka, ``); r.m["status"] != "archived" {
			t.Fatalf("archive %s", r.body)
		}
	}
	if r := e.do(t, nil, "GET", "/v1/web/public/blog/pages/news/hello", "", ""); r.status != 404 {
		t.Fatalf("archived is public %d", r.status)
	}
	if r := e.do(t, nil, "POST", "/v1/web/pages/"+id+"/publish", ka, `{}`); r.m["status"] != "published" {
		t.Fatalf("republish archived %s", r.body)
	}
	if r := e.do(t, nil, "GET", "/v1/web/sites/blog/pages?status=published", ka, ""); len(r.m["pages"].([]any)) != 1 {
		t.Fatalf("list %s", r.body)
	}
}

func TestScheduledPublish(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	_, err := e.k.CreateSite(ctx, "ta", web.Site{ID: "s1", Name: "S", BaseURL: "https://s.example.com"})
	must(t, err)
	p, err := e.k.CreatePage(ctx, "ta", "s1", "test", web.PageInput{Slug: "launch", Title: "Launch"})
	must(t, err)
	at := e.clk.Now().Add(time.Hour)
	p, err = e.k.Publish(ctx, "ta", p.ID, 0, at)
	must(t, err)
	if p.Status != web.StatusScheduled || p.ScheduledRevision != 1 || *p.PublishAt != at.UnixMilli() {
		t.Fatalf("scheduled %+v", p)
	}
	if errs := e.runJobs(t); len(errs) != 0 {
		t.Fatalf("job ran early: %v", errs)
	}
	if _, err := e.k.PublishedPage(ctx, "s1", "launch"); code(err) != "not_found" {
		t.Fatalf("scheduled page visible: %v", err)
	}
	e.clk.Add(time.Hour)
	if errs := e.runJobs(t); len(errs) != 0 {
		t.Fatal(errs)
	}
	got, err := e.k.GetPage(ctx, "ta", p.ID)
	must(t, err)
	if got.Status != web.StatusPublished || got.PublishedRevision != 1 {
		t.Fatalf("after due %+v", got)
	}
	// Unschedule / reschedule: the stale job does nothing.
	p2, err := e.k.CreatePage(ctx, "ta", "s1", "test", web.PageInput{Slug: "later", Title: "Later"})
	must(t, err)
	_, err = e.k.Publish(ctx, "ta", p2.ID, 0, e.clk.Now().Add(time.Minute))
	must(t, err)
	_, err = e.k.Unschedule(ctx, "ta", p2.ID)
	must(t, err)
	_, err = e.k.Publish(ctx, "ta", p2.ID, 0, e.clk.Now().Add(2*time.Hour))
	must(t, err)
	e.clk.Add(time.Minute)
	if errs := e.runJobs(t); len(errs) != 0 {
		t.Fatal(errs)
	}
	if got, _ := e.k.GetPage(ctx, "ta", p2.ID); got.Status != web.StatusScheduled {
		t.Fatalf("stale job published the page: %s", got.Status)
	}
	e.clk.Add(2 * time.Hour)
	e.runJobs(t)
	if got, _ := e.k.GetPage(ctx, "ta", p2.ID); got.Status != web.StatusPublished {
		t.Fatalf("rescheduled job: %s", got.Status)
	}
}

func TestSitemapAndRobots(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	_, err := e.k.CreateSite(ctx, "ta", web.Site{ID: "docs", Name: "Docs", BaseURL: "https://docs.example.com", RobotsDisallow: []string{"/private"}})
	must(t, err)
	for _, in := range []web.PageInput{
		{Slug: "b", Title: "B", SEO: web.SEO{CanonicalURL: "https://example.com/canonical-b"}},
		{Slug: "a", Title: "A"},
		{Slug: "hidden", Title: "H", SEO: web.SEO{NoIndex: true}},
		{Slug: "draft", Title: "D"},
	} {
		p, err := e.k.CreatePage(ctx, "ta", "docs", "t", in)
		must(t, err)
		if in.Slug != "draft" {
			_, err = e.k.Publish(ctx, "ta", p.ID, 0, time.Time{})
			must(t, err)
		}
	}
	r := e.do(t, nil, "GET", "/v1/web/public/docs/sitemap.xml", "", "")
	var set struct {
		URLs []struct {
			Loc     string `xml:"loc"`
			LastMod string `xml:"lastmod"`
		} `xml:"url"`
	}
	must(t, xml.Unmarshal(r.body, &set))
	if r.status != 200 || !strings.HasPrefix(r.hdr.Get("Content-Type"), "application/xml") || len(set.URLs) != 2 ||
		set.URLs[0].Loc != "https://docs.example.com/a" || set.URLs[1].Loc != "https://example.com/canonical-b" || set.URLs[0].LastMod == "" {
		t.Fatalf("sitemap %d %s", r.status, r.body)
	}
	if r2 := e.do(t, nil, "GET", "/v1/web/public/docs/sitemap.xml", "", "", "If-None-Match", r.hdr.Get("ETag")); r2.status != 304 {
		t.Fatalf("sitemap 304: %d", r2.status)
	}
	if r := e.do(t, nil, "GET", "/v1/web/public/docs/pages/hidden", "", ""); r.status != 200 || r.hdr.Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("noindex page %d %v", r.status, r.hdr)
	}
	r = e.do(t, nil, "GET", "/v1/web/public/docs/robots.txt", "", "")
	want := "User-agent: *\nDisallow: /private\n\nSitemap: https://docs.example.com/sitemap.xml\n"
	if r.status != 200 || string(r.body) != want {
		t.Fatalf("robots %d %q", r.status, r.body)
	}
	if r := e.do(t, nil, "GET", "/v1/web/public/nope/robots.txt", "", ""); r.status != 404 {
		t.Fatalf("unknown site %d", r.status)
	}
}

func (e *env) account(t *testing.T, tenant, id string, credit int64) {
	t.Helper()
	ctx := context.Background()
	_, err := e.b.CreateAccount(ctx, tenant, "acct-"+id, id, "USD")
	must(t, err)
	if credit > 0 {
		_, err = e.b.Credit(ctx, tenant, "credit-"+id, id, credit, "")
		must(t, err)
	}
}

func (e *env) balance(t *testing.T, tenant, id string) int64 {
	t.Helper()
	a, err := e.b.GetAccount(context.Background(), tenant, id)
	must(t, err)
	return a.Available
}

func TestOrderStateMachine(t *testing.T) {
	e := newEnv(t, false)
	ka := e.key(t, "ta")
	e.account(t, "ta", "wallet", 1000)
	body := `{"account_id":"wallet","amount":300,"customer_ref":"c1","items":[{"sku":"x","qty":1}]}`
	r := e.do(t, nil, "POST", "/v1/web/orders", ka, body, "Idempotency-Key", "o1")
	if r.status != 201 || r.m["status"] != "created" || r.m["currency"] != "USD" {
		t.Fatalf("create %d %s", r.status, r.body)
	}
	id := r.m["id"].(string)
	if r2 := e.do(t, nil, "POST", "/v1/web/orders", ka, body, "Idempotency-Key", "o1"); r2.m["id"] != id || r2.hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("idempotent create %s", r2.body)
	}
	if r := e.do(t, nil, "POST", "/v1/web/orders", ka, body); r.status != 400 {
		t.Fatalf("missing key %d", r.status)
	}
	if r := e.do(t, nil, "POST", "/v1/web/orders", ka, `{"account_id":"nope","amount":1}`, "Idempotency-Key", "o2"); r.status != 404 {
		t.Fatalf("unknown account %d", r.status)
	}
	if r := e.do(t, nil, "POST", "/v1/web/orders/"+id+"/fulfill", ka, ""); r.code() != "invalid_transition" {
		t.Fatalf("fulfill unpaid %s", r.body)
	}
	for i := 0; i < 2; i++ { // paying twice charges once
		if r := e.do(t, nil, "POST", "/v1/web/orders/"+id+"/pay", ka, ""); r.status != 200 || r.m["status"] != "paid" {
			t.Fatalf("pay %d %s", r.status, r.body)
		}
	}
	if b := e.balance(t, "ta", "wallet"); b != 700 {
		t.Fatalf("balance after pay %d", b)
	}
	for i := 0; i < 2; i++ {
		if r := e.do(t, nil, "POST", "/v1/web/orders/"+id+"/fulfill", ka, ""); r.m["status"] != "fulfilled" {
			t.Fatalf("fulfill %s", r.body)
		}
	}
	if r := e.do(t, nil, "POST", "/v1/web/orders/"+id+"/cancel", ka, ""); r.code() != "invalid_transition" {
		t.Fatalf("cancel fulfilled %s", r.body)
	}
	for i := 0; i < 2; i++ { // refunding twice credits once
		if r := e.do(t, nil, "POST", "/v1/web/orders/"+id+"/refund", ka, ""); r.m["status"] != "refunded" {
			t.Fatalf("refund %s", r.body)
		}
	}
	if b := e.balance(t, "ta", "wallet"); b != 1000 {
		t.Fatalf("balance after refund %d", b)
	}
	r = e.do(t, nil, "GET", "/v1/web/orders/"+id+"/events", ka, "")
	var types []string
	for _, ev := range r.m["events"].([]any) {
		types = append(types, ev.(map[string]any)["type"].(string))
	}
	if strings.Join(types, ",") != "order.created,order.paid,order.fulfilled,order.refunded" {
		t.Fatalf("events %v", types)
	}
	// Insufficient funds leaves the order created.
	big := e.do(t, nil, "POST", "/v1/web/orders", ka, `{"account_id":"wallet","amount":5000}`, "Idempotency-Key", "o3")
	if r := e.do(t, nil, "POST", "/v1/web/orders/"+big.m["id"].(string)+"/pay", ka, ""); r.code() != "insufficient_funds" {
		t.Fatalf("insufficient %s", r.body)
	}
	if o, _ := e.k.GetOrder(context.Background(), "ta", big.m["id"].(string)); o.Status != web.OrderCreated {
		t.Fatalf("status %s", o.Status)
	}
	// Cancel, then pay: rejected, no charge.
	c := e.do(t, nil, "POST", "/v1/web/orders", ka, `{"account_id":"wallet","amount":100}`, "Idempotency-Key", "o4")
	cid := c.m["id"].(string)
	for i := 0; i < 2; i++ {
		if r := e.do(t, nil, "POST", "/v1/web/orders/"+cid+"/cancel", ka, ""); r.m["status"] != "cancelled" {
			t.Fatalf("cancel %s", r.body)
		}
	}
	if r := e.do(t, nil, "POST", "/v1/web/orders/"+cid+"/pay", ka, ""); r.code() != "invalid_transition" {
		t.Fatalf("pay cancelled %s", r.body)
	}
	if b := e.balance(t, "ta", "wallet"); b != 1000 {
		t.Fatalf("balance %d", b)
	}
}

// A payment interrupted after the charge settled (crash before the order
// became paid) is compensated exactly once when the order is cancelled, even
// if the interrupted pay is retried afterwards.
func TestOrderCompensationAfterInterruptedPay(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	e.account(t, "ta", "wallet", 1000)
	o := createOrder(t, e, "ta", "wallet", 400)
	// Simulate the first half of Pay: reserve + settle with the order's keys.
	res, err := e.b.Reserve(ctx, "ta", "web-order:"+o.ID+":reserve", "wallet", 400, 0, "web order "+o.ID)
	must(t, err)
	_, err = e.b.Settle(ctx, "ta", "web-order:"+o.ID+":settle", res.Reservation.ID, 400, "web order "+o.ID)
	must(t, err)
	must(t, e.k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE web_orders SET reservation_id=? WHERE tenant_id=? AND id=?`, res.Reservation.ID, "ta", o.ID)
		return err
	}))
	if b := e.balance(t, "ta", "wallet"); b != 600 {
		t.Fatalf("charged balance %d", b)
	}
	got, err := e.k.Cancel(ctx, "ta", o.ID)
	must(t, err)
	if got.Status != web.OrderCancelled || e.balance(t, "ta", "wallet") != 1000 {
		t.Fatalf("cancel did not refund: %s %d", got.Status, e.balance(t, "ta", "wallet"))
	}
	if _, err := e.k.Pay(ctx, "ta", o.ID); code(err) != "invalid_transition" {
		t.Fatalf("retried pay: %v", err)
	}
	if _, err := e.k.Cancel(ctx, "ta", o.ID); err != nil {
		t.Fatal(err)
	}
	if b := e.balance(t, "ta", "wallet"); b != 1000 {
		t.Fatalf("refunded twice: %d", b)
	}
	// A reservation that was only held is released.
	o2 := createOrder(t, e, "ta", "wallet", 100)
	res2, err := e.b.Reserve(ctx, "ta", "web-order:"+o2.ID+":reserve", "wallet", 100, 0, "web order "+o2.ID)
	must(t, err)
	must(t, e.k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE web_orders SET reservation_id=? WHERE tenant_id=? AND id=?`, res2.Reservation.ID, "ta", o2.ID)
		return err
	}))
	_, err = e.k.Cancel(ctx, "ta", o2.ID)
	must(t, err)
	if r, _ := e.b.GetReservation(ctx, "ta", res2.Reservation.ID); r.Status != billing.StatusReleased || e.balance(t, "ta", "wallet") != 1000 {
		t.Fatalf("reservation %s balance %d", r.Status, e.balance(t, "ta", "wallet"))
	}
}

func createOrder(t *testing.T, e *env, tenant, acct string, amount int64) web.Order {
	t.Helper()
	r := e.do(t, nil, "POST", "/v1/web/orders", e.key(t, tenant), `{"account_id":"`+acct+`","amount":`+strconv.FormatInt(amount, 10)+`}`,
		"Idempotency-Key", httpx.RandomID("k"))
	if r.status != 201 {
		t.Fatalf("create order %d %s", r.status, r.body)
	}
	o, err := e.k.GetOrder(context.Background(), tenant, r.m["id"].(string))
	must(t, err)
	return o
}

func TestWebhookSigningRetryAndIdempotency(t *testing.T) {
	e := newEnv(t, false)
	e.k.Client = web.NewWebhookClient(true) // the test receiver listens on loopback
	e.k.Cfg.WebhookAllowPrivateNetworks = true
	ctx := context.Background()
	type hit struct {
		id, ts, sig string
		body        []byte
	}
	var mu sync.Mutex
	var hits []hit
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, hit{r.Header.Get(web.HeaderEventID), r.Header.Get(web.HeaderTimestamp), r.Header.Get(web.HeaderSignature), b})
		n := len(hits)
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer recv.Close()
	h, err := e.k.CreateWebhook(ctx, "ta", recv.URL+"/hook", []string{"order.paid"})
	must(t, err)
	if !strings.HasPrefix(h.Secret, "whsec_") {
		t.Fatalf("secret %q", h.Secret)
	}
	if list, _ := e.k.ListWebhooks(ctx, "ta"); len(list) != 1 || list[0].Secret != "" {
		t.Fatalf("list leaks secret: %+v", list)
	}
	e.account(t, "ta", "wallet", 1000)
	o := createOrder(t, e, "ta", "wallet", 10) // order.created: not subscribed
	_, err = e.k.Pay(ctx, "ta", o.ID)
	must(t, err)
	for i := 0; i < 3; i++ {
		e.runJobs(t)
		e.clk.Add(2 * time.Hour) // past any retry backoff
	}
	ds, err := e.k.Deliveries(ctx, "ta", h.ID)
	must(t, err)
	if len(ds) != 1 || ds[0].Status != "delivered" || ds[0].Attempts != 3 || ds[0].LastStatus != 204 {
		t.Fatalf("deliveries %+v", ds)
	}
	mu.Lock()
	if len(hits) != 3 {
		t.Fatalf("%d requests", len(hits))
	}
	for _, x := range hits {
		ts, _ := strconv.ParseInt(x.ts, 10, 64)
		if x.id != ds[0].EventID || !web.Verify(h.Secret, x.sig, ts, x.body, time.Unix(ts, 0), 5*time.Minute) ||
			web.Verify("whsec_wrong", x.sig, ts, x.body, time.Unix(ts, 0), 5*time.Minute) {
			t.Fatalf("bad delivery %+v", x)
		}
		var ev map[string]any
		must(t, json.Unmarshal(x.body, &ev))
		if ev["type"] != "order.paid" || ev["id"] != x.id || ev["data"].(map[string]any)["id"] != o.ID {
			t.Fatalf("payload %s", x.body)
		}
	}
	mu.Unlock()
	// A duplicate execution of the delivery job does not send again.
	payload, _ := json.Marshal(map[string]string{"event_id": ds[0].EventID, "webhook_id": h.ID})
	must(t, e.k.JobHandlers()[web.JobWebhook](ctx, &jobs.Claimed{TenantID: "ta", Type: web.JobWebhook, Payload: payload, Attempt: 9}))
	mu.Lock()
	if len(hits) != 3 {
		t.Fatalf("duplicate job re-sent: %d", len(hits))
	}
	mu.Unlock()

	// The default client refuses private/loopback targets (SSRF guard), and
	// the last attempt marks the delivery failed.
	e.k.Client = nil
	e.k.Cfg.WebhookAllowPrivateNetworks = false
	e.k.Cfg.WebhookMaxAttempts = 1
	// (A literal loopback IP is refused at creation; a host name is checked
	// on its resolved address when delivering.)
	h2, err := e.k.CreateWebhook(ctx, "ta", strings.Replace(recv.URL, "127.0.0.1", "localhost", 1)+"/other", nil)
	must(t, err)
	o2 := createOrder(t, e, "ta", "wallet", 10)
	errs := e.runJobs(t)
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "not a public address") {
		t.Fatalf("ssrf guard: %v", errs)
	}
	ds2, _ := e.k.Deliveries(ctx, "ta", h2.ID)
	if len(ds2) != 1 || ds2[0].Status != "failed" || !strings.Contains(ds2[0].LastError, "not a public address") {
		t.Fatalf("failed delivery %+v (order %s)", ds2, o2.ID)
	}
}

func TestTenantIsolationAndEditorSessions(t *testing.T) {
	e := newEnv(t, true)
	ka, kb := e.key(t, "default"), e.key(t, "other")
	e.do(t, nil, "POST", "/v1/web/sites", ka, `{"id":"main","name":"Main","base_url":"https://example.com"}`)
	p := e.do(t, nil, "POST", "/v1/web/sites/main/pages", ka, `{"slug":"about","title":"About"}`)
	pid := p.m["id"].(string)
	e.account(t, "default", "wallet", 100)
	o := e.do(t, nil, "POST", "/v1/web/orders", ka, `{"account_id":"wallet","amount":5}`, "Idempotency-Key", "x")
	oid := o.m["id"].(string)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/web/pages/" + pid, ""},
		{"PUT", "/v1/web/pages/" + pid, `{"slug":"about","title":"x","expected_version":1}`},
		{"POST", "/v1/web/pages/" + pid + "/publish", `{}`},
		{"POST", "/v1/web/sites/main/pages", `{"slug":"evil","title":"x"}`},
		{"GET", "/v1/web/sites/main/pages", ""},
		{"GET", "/v1/web/orders/" + oid, ""},
		{"POST", "/v1/web/orders/" + oid + "/pay", ""},
	} {
		if r := e.do(t, nil, c.method, c.path, kb, c.body); r.status != 404 {
			t.Errorf("other tenant %s %s: %d %s", c.method, c.path, r.status, r.body)
		}
	}
	if r := e.do(t, nil, "GET", "/v1/web/orders", kb, ""); len(r.m["orders"].([]any)) != 0 {
		t.Fatalf("other tenant list %s", r.body)
	}
	if r := e.do(t, nil, "GET", "/v1/web/sites", e.key(t, "default", "billing"), ""); r.status != 403 {
		t.Fatalf("scope %d", r.status)
	}
	if r := e.do(t, nil, "GET", "/v1/web/sites", "", ""); r.status != 401 {
		t.Fatalf("anonymous %d", r.status)
	}

	// Editor web session: login works, but only listed editors may edit.
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	su := e.do(t, c, "POST", "/v1/auth/signup", "", `{"email":"ed@example.com","password":"correct horse battery"}`)
	if su.status != 201 {
		t.Fatalf("signup %d %s", su.status, su.body)
	}
	csrf := su.m["csrf_token"].(string)
	uid := su.m["user"].(map[string]any)["id"].(string)
	if r := e.do(t, c, "GET", "/v1/web/sites", "", ""); r.status != 403 {
		t.Fatalf("non-editor %d %s", r.status, r.body)
	}
	if r := e.do(t, c, "POST", "/v1/web/editors", "", `{"user_id":"`+uid+`"}`, "X-CSRF-Token", csrf); r.status != 403 {
		t.Fatalf("session may not manage editors: %d", r.status)
	}
	if r := e.do(t, nil, "POST", "/v1/web/editors", ka, `{"user_id":"`+uid+`"}`); r.status != 200 {
		t.Fatalf("add editor %d %s", r.status, r.body)
	}
	if r := e.do(t, c, "GET", "/v1/web/sites", "", ""); r.status != 200 || len(r.m["sites"].([]any)) != 1 {
		t.Fatalf("editor list %d %s", r.status, r.body)
	}
	if r := e.do(t, c, "POST", "/v1/web/sites/main/pages", "", `{"slug":"team","title":"Team"}`); r.code() != "csrf_failed" {
		t.Fatalf("missing csrf %d %s", r.status, r.body)
	}
	r := e.do(t, c, "POST", "/v1/web/sites/main/pages", "", `{"slug":"team","title":"Team"}`, "X-CSRF-Token", csrf)
	if r.status != 201 {
		t.Fatalf("editor create %d %s", r.status, r.body)
	}
	rev, _ := e.k.Revisions(context.Background(), "default", r.m["id"].(string))
	if rev[0].Author != "user:"+uid {
		t.Fatalf("author %q", rev[0].Author)
	}
	if r := e.do(t, nil, "DELETE", "/v1/web/editors/"+uid, ka, ""); r.status != 204 {
		t.Fatalf("remove editor %d", r.status)
	}
	if r := e.do(t, c, "GET", "/v1/web/sites", "", ""); r.status != 403 {
		t.Fatalf("removed editor %d", r.status)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func code(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "untyped:" + err.Error()
}
