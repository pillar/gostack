package commerce_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/kits/game/commerce"
	"github.com/pillar/gostack/modules/jobs"
)

// testClock is an adjustable clock for the job queue.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.t) {
		c.t = t
	}
}

func (f *gfixture) setFinishErr(status int) {
	f.g.mu.Lock()
	f.g.finishErr = status
	f.g.mu.Unlock()
}

// deliverGoogle verifies a fresh purchase and returns its order id.
func (f *gfixture) deliverGoogle(token string) string {
	f.T.Helper()
	p := f.Guest("alpha")
	f.g.purchase(token, "gems_100", f.order("alpha", p))
	r := f.Expect(f.verify("alpha", p, "gems_100", token), 200)
	return r.Body["order"].(map[string]any)["order_id"].(string)
}

// finishJob returns the order's finish job (id, status, attempts, run_at).
func (f *gfixture) finishJob(orderID string) (id, status string, attempts int, runAt int64) {
	f.T.Helper()
	err := f.App.DB.Read.QueryRow(`SELECT id, status, attempts, run_at FROM jobs WHERE type=? AND dedupe_key=?`,
		commerce.JobGoogleFinish, "finish:"+orderID).Scan(&id, &status, &attempts, &runAt)
	if err == sql.ErrNoRows {
		return "", "", 0, 0
	}
	if err != nil {
		f.T.Fatal(err)
	}
	return
}

func (f *gfixture) adminOrder(orderID string) map[string]any {
	f.T.Helper()
	r := f.Expect(f.Do("GET", "/v1/admin/games/alpha/orders?limit=200", f.AdminToken, nil), 200)
	for _, o := range r.Body["orders"].([]any) {
		if m := o.(map[string]any); m["order_id"] == orderID {
			return m
		}
	}
	f.T.Fatalf("order %s not listed", orderID)
	return nil
}

// runOne claims the next finish job, runs it and acks/nacks it like the worker.
func (f *gfixture) runOne(q *jobs.Queue) error {
	f.T.Helper()
	ctx := context.Background()
	c, err := q.Claim(ctx, []string{commerce.JobGoogleFinish}, time.Minute)
	if err != nil || c == nil {
		f.T.Fatalf("claim: %v %v", c, err)
	}
	herr := f.mod.JobHandlers()[commerce.JobGoogleFinish](ctx, c)
	if herr == nil {
		err = q.Ack(ctx, c)
	} else {
		err = q.Nack(ctx, c, herr)
	}
	if err != nil {
		f.T.Fatal(err)
	}
	return herr
}

// A Google Play outage much longer than the default retry budget (5 attempts,
// ~30 s) must not kill the acknowledge/consume job: Google refunds purchases
// that are not acknowledged within three days.
func TestGoogleFinishSurvivesLongOutage(t *testing.T) {
	f := setupGoogle(t)
	clock := &testClock{t: time.Now()}
	f.App.Jobs.Now = clock.Now
	id := f.deliverGoogle("long-1")
	if o := f.adminOrder(id); o["finish_state"] != "pending" {
		t.Fatalf("%v", o)
	}
	f.setFinishErr(503)
	start, attempts := clock.Now(), 0
	for clock.Now().Sub(start) < 48*time.Hour {
		_, status, _, runAt := f.finishJob(id)
		if status != "pending" && status != "retry_wait" {
			t.Fatalf("after %d attempts over %v the job is %q", attempts, clock.Now().Sub(start), status)
		}
		clock.Set(time.UnixMilli(runAt))
		if err := f.runOne(f.App.Jobs); err == nil {
			t.Fatal("finish succeeded during the outage")
		}
		attempts++
	}
	if _, status, _, _ := f.finishJob(id); status != "retry_wait" {
		t.Fatalf("job %s after 48h of 503", status)
	}
	t.Logf("still retryable after %d attempts over %v", attempts, clock.Now().Sub(start))
	// The store recovers: the next attempt completes and is recorded on the order.
	f.setFinishErr(0)
	_, _, _, runAt := f.finishJob(id)
	clock.Set(time.UnixMilli(runAt))
	if err := f.runOne(f.App.Jobs); err != nil {
		t.Fatal(err)
	}
	if _, status, _, _ := f.finishJob(id); status != "succeeded" {
		t.Fatal(status)
	}
	o := f.adminOrder(id)
	if o["finish_state"] != "done" || o["finished_at"] == nil {
		t.Fatalf("%v", o)
	}
}

// Maintain re-enqueues the finish job of a delivered order that is not
// finished when the job is dead (e.g. created with an older, short budget) or
// missing; finished orders are left alone.
func TestGoogleFinishReconciliation(t *testing.T) {
	f := setupGoogle(t)
	ctx := context.Background()
	dead, missing := f.deliverGoogle("rec-1"), f.deliverGoogle("rec-2")
	jid, _, _, _ := f.finishJob(dead)
	err := f.App.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='dead', attempts=max_attempts WHERE id=?`, jid); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE dedupe_key=?`, "finish:"+missing)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.mod.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, st, att, _ := f.finishJob(dead); st != "pending" || att != 0 {
		t.Fatalf("dead job not revived: %s %d", st, att)
	}
	if id, st, _, _ := f.finishJob(missing); id == "" || st != "pending" {
		t.Fatalf("missing job not re-enqueued: %q %s", id, st)
	}
	if n := f.runFinishJobs(); n != 2 {
		t.Fatalf("ran %d", n)
	}
	for _, id := range []string{dead, missing} {
		if o := f.adminOrder(id); o["finish_state"] != "done" {
			t.Fatalf("%v", o)
		}
	}
	f.g.mu.Lock()
	got := len(f.g.finished)
	f.g.mu.Unlock()
	if got != 2 {
		t.Fatalf("finished=%d", got)
	}
	// Nothing left to reconcile.
	if err := f.mod.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.runFinishJobs(); n != 0 {
		t.Fatalf("ran %d after reconciliation", n)
	}
}

// A non-retryable rejection (403: the service account lacks permission) is
// surfaced as needs_attention (order flag, admin filter, metric) and keeps
// retrying, so the purchase is finished once the operator fixes the cause.
func TestGoogleFinishNeedsAttention(t *testing.T) {
	f := setupGoogle(t)
	id := f.deliverGoogle("att-1")
	f.setFinishErr(403)
	if err := f.runOne(f.App.Jobs); err == nil {
		t.Fatal("403 treated as success")
	}
	if _, st, _, _ := f.finishJob(id); st != "retry_wait" {
		t.Fatalf("job %s", st)
	}
	r := f.Expect(f.Do("GET", "/v1/admin/games/alpha/orders?finish_state=needs_attention", f.AdminToken, nil), 200)
	list := r.Body["orders"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["order_id"] != id || list[0].(map[string]any)["finish_error"] != "google play api: HTTP 403" {
		t.Fatalf("%s", r.Raw)
	}
	if n := f.App.Metrics.Counter("commerce_store_finish_attention_total", `provider="google",status="403"`).Load(); n != 1 {
		t.Fatalf("metric=%d", n)
	}
	wantCode(t, f.Do("GET", "/v1/admin/games/alpha/orders?finish_state=bogus", f.AdminToken, nil), 400, "invalid_request")
	// Fixed: the next attempt finishes it and clears the flag.
	f.setFinishErr(0)
	f.App.DB.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE jobs SET run_at=0 WHERE dedupe_key=?`, "finish:"+id)
		return err
	})
	if err := f.runOne(f.App.Jobs); err != nil {
		t.Fatal(err)
	}
	if o := f.adminOrder(id); o["finish_state"] != "done" || o["finish_error"] != nil {
		t.Fatalf("%v", o)
	}
	// A rejection because the purchase is already consumed counts as done.
	id2 := f.deliverGoogle("att-2")
	f.g.mu.Lock()
	f.g.purchases["att-2"]["consumptionState"] = 1
	f.g.finishErr = 400
	f.g.mu.Unlock()
	if err := f.runOne(f.App.Jobs); err != nil {
		t.Fatal(err)
	}
	if o := f.adminOrder(id2); o["finish_state"] != "done" {
		t.Fatalf("%v", o)
	}
}
