package billing_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/billing"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newService(t *testing.T) (*billing.Service, *clock) {
	t.Helper()
	o := sqlite.Options{Path: filepath.Join(t.TempDir(), "app.db"), ReadConnections: 2, Synchronous: "FULL",
		BusyTimeout: time.Second, WALAutocheckpoint: 1000, WriteQueue: 256}
	db := pgtest.Open(t, o)
	c := &clock{t: time.UnixMilli(1_700_000_000_000)}
	return &billing.Service{DB: db, Now: c.Now}, c
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

func must(t *testing.T) func(billing.Result, error) billing.Result {
	return func(r billing.Result, err error) billing.Result {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
}

func setup(t *testing.T, s *billing.Service, tenant, id string, balance int64) {
	t.Helper()
	ctx := context.Background()
	must(t)(s.CreateAccount(ctx, tenant, "create-"+id, id, "CREDIT"))
	if balance > 0 {
		must(t)(s.Credit(ctx, tenant, "topup-"+id, id, balance, "initial"))
	}
}

func assertBooks(t *testing.T, s *billing.Service) {
	t.Helper()
	ms, err := s.Reconcile(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 0 {
		t.Fatalf("reconcile mismatches: %+v", ms)
	}
}

// Concurrent debits and reservations never overdraw, and the books balance.
func TestConcurrentNoOverdraw(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	setup(t, s, "t1", "a", 100)
	var ok, insufficient atomic.Int64
	var wg sync.WaitGroup
	for i := range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = s.Debit(ctx, "t1", fmt.Sprintf("d%d", i), "a", 7, "")
			} else {
				_, err = s.Reserve(ctx, "t1", fmt.Sprintf("r%d", i), "a", 7, time.Minute, "")
			}
			switch code(err) {
			case "":
				ok.Add(1)
			case "insufficient_funds":
				insufficient.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	a, err := s.GetAccount(ctx, "t1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if ok.Load() != 14 || insufficient.Load() != 46 {
		t.Fatalf("ok=%d insufficient=%d, want 14/46", ok.Load(), insufficient.Load())
	}
	if a.Available != 100-14*7 || a.Available < 0 {
		t.Fatalf("available=%d", a.Available)
	}
	assertBooks(t, s)
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	setup(t, s, "t1", "a", 100)

	first := must(t)(s.Debit(ctx, "t1", "k1", "a", 30, "order 1"))
	again := must(t)(s.Debit(ctx, "t1", "k1", "a", 30, "order 1"))
	if first.Replayed || !again.Replayed || again.Entry.ID != first.Entry.ID || again.Account.Available != 70 {
		t.Fatalf("replay: first=%+v again=%+v", first, again)
	}
	if _, err := s.Debit(ctx, "t1", "k1", "a", 31, "order 1"); code(err) != "idempotency_key_reused" {
		t.Fatalf("different amount: %v", err)
	}
	if _, err := s.Credit(ctx, "t1", "k1", "a", 30, "order 1"); code(err) != "idempotency_key_reused" {
		t.Fatalf("different op: %v", err)
	}
	// Keys are per tenant: another tenant may use k1.
	setup(t, s, "t2", "a", 5)
	must(t)(s.Debit(ctx, "t2", "k1", "a", 5, "order 1"))

	// Concurrent duplicates of one key charge exactly once.
	var wg sync.WaitGroup
	ids := make([]string, 20)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Debit(ctx, "t1", "dup", "a", 10, "")
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = r.Entry.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different entries for one key: %v", ids)
		}
	}
	if a, _ := s.GetAccount(ctx, "t1", "a"); a.Available != 60 {
		t.Fatalf("available=%d, want 60", a.Available)
	}
	// A failed attempt is not recorded; the key may be retried after a top-up.
	if _, err := s.Debit(ctx, "t1", "big", "a", 500, ""); code(err) != "insufficient_funds" {
		t.Fatal(err)
	}
	must(t)(s.Credit(ctx, "t1", "topup2", "a", 1000, ""))
	must(t)(s.Debit(ctx, "t1", "big", "a", 500, ""))
	if _, err := s.Debit(ctx, "t1", "sys:x", "a", 1, ""); code(err) != "invalid_request" {
		t.Fatalf("reserved key prefix: %v", err)
	}
	assertBooks(t, s)
}

func TestReservationStateMachine(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	setup(t, s, "t1", "a", 100)

	// Partial settle releases the remainder.
	r := must(t)(s.Reserve(ctx, "t1", "r1", "a", 60, 0, "llm call"))
	if r.Account.Available != 40 || r.Account.Reserved != 60 || r.Reservation.Status != billing.StatusReserved {
		t.Fatalf("after reserve: %+v", r)
	}
	if _, err := s.Settle(ctx, "t1", "s1x", r.Reservation.ID, 61, ""); code(err) != "invalid_request" {
		t.Fatalf("settle > reserved: %v", err)
	}
	st := must(t)(s.Settle(ctx, "t1", "s1", r.Reservation.ID, 25, ""))
	if st.Account.Available != 75 || st.Account.Reserved != 0 || st.Reservation.Status != billing.StatusSettled || *st.Reservation.SettledAmount != 25 {
		t.Fatalf("after settle: %+v %+v", st.Account, st.Reservation)
	}
	// Illegal transitions from settled.
	if _, err := s.Settle(ctx, "t1", "s1b", r.Reservation.ID, 1, ""); code(err) != "invalid_state" {
		t.Fatalf("double settle: %v", err)
	}
	if _, err := s.Release(ctx, "t1", "rel1", r.Reservation.ID, ""); code(err) != "invalid_state" {
		t.Fatalf("release settled: %v", err)
	}
	if _, err := s.MarkUncertain(ctx, "t1", "u1", r.Reservation.ID, ""); code(err) != "invalid_state" {
		t.Fatalf("uncertain settled: %v", err)
	}

	// Release then illegal settle.
	r2 := must(t)(s.Reserve(ctx, "t1", "r2", "a", 10, 0, ""))
	rel := must(t)(s.Release(ctx, "t1", "rel2", r2.Reservation.ID, ""))
	if rel.Account.Available != 75 || rel.Reservation.Status != billing.StatusReleased {
		t.Fatalf("after release: %+v", rel)
	}
	if _, err := s.Settle(ctx, "t1", "s2", r2.Reservation.ID, 5, ""); code(err) != "invalid_state" {
		t.Fatalf("settle released: %v", err)
	}

	// Uncertain → reconciling: no settle/release/resolve-on-reserved shortcuts.
	r3 := must(t)(s.Reserve(ctx, "t1", "r3", "a", 20, 0, ""))
	if _, err := s.ResolveReconciliation(ctx, "t1", "res0", r3.Reservation.ID, "settle", 5, "op"); code(err) != "invalid_state" {
		t.Fatalf("resolve while reserved: %v", err)
	}
	u := must(t)(s.MarkUncertain(ctx, "t1", "u3", r3.Reservation.ID, "timeout from provider"))
	if u.Reservation.Status != billing.StatusReconciling || u.Account.Reserved != 20 || u.Entry != nil {
		t.Fatalf("after uncertain: %+v", u)
	}
	if _, err := s.Settle(ctx, "t1", "s3", r3.Reservation.ID, 5, ""); code(err) != "invalid_state" {
		t.Fatalf("settle reconciling: %v", err)
	}
	if _, err := s.Release(ctx, "t1", "rel3", r3.Reservation.ID, ""); code(err) != "invalid_state" {
		t.Fatalf("release reconciling: %v", err)
	}
	if _, err := s.ResolveReconciliation(ctx, "t1", "res3", r3.Reservation.ID, "settle", 5, ""); code(err) != "invalid_request" {
		t.Fatalf("resolve without note: %v", err)
	}
	if _, err := s.ResolveReconciliation(ctx, "t1", "res3", r3.Reservation.ID, "refund", 0, "x"); code(err) != "invalid_request" {
		t.Fatalf("bad action: %v", err)
	}
	fin := must(t)(s.ResolveReconciliation(ctx, "t1", "res3", r3.Reservation.ID, "settle", 12, "provider confirmed 12"))
	if fin.Account.Available != 75-12 || fin.Account.Reserved != 0 || fin.Reservation.Status != billing.StatusSettled || fin.Entry.Note != "provider confirmed 12" {
		t.Fatalf("after resolve: %+v %+v", fin.Account, fin.Reservation)
	}
	if _, err := s.ResolveReconciliation(ctx, "t1", "res3b", r3.Reservation.ID, "release", 0, "again"); code(err) != "invalid_state" {
		t.Fatalf("resolve twice: %v", err)
	}

	// Unknown reservation and cross-tenant access.
	if _, err := s.Settle(ctx, "t2", "x", r3.Reservation.ID, 1, ""); code(err) != "not_found" {
		t.Fatalf("cross tenant: %v", err)
	}
	if _, err := s.Debit(ctx, "t1", "nf", "nope", 1, ""); code(err) != "not_found" {
		t.Fatalf("missing account: %v", err)
	}
	assertBooks(t, s)
	es, err := s.Entries(ctx, "t1", "a", 100)
	if err != nil {
		t.Fatal(err)
	}
	// credit, reserve, settle, reserve, release, reserve, settle(resolve)
	if len(es) != 7 || es[0].Type != "settle" || es[len(es)-1].Type != "credit" {
		t.Fatalf("entries: %+v", es)
	}
}

func TestExpirySweeper(t *testing.T) {
	s, clk := newService(t)
	ctx := context.Background()
	setup(t, s, "t1", "a", 100)
	r1 := must(t)(s.Reserve(ctx, "t1", "r1", "a", 30, time.Minute, ""))
	r2 := must(t)(s.Reserve(ctx, "t1", "r2", "a", 20, time.Minute, ""))
	r3 := must(t)(s.Reserve(ctx, "t1", "r3", "a", 10, time.Hour, ""))
	must(t)(s.MarkUncertain(ctx, "t1", "u2", r2.Reservation.ID, ""))

	if n, err := s.ExpireReservations(ctx, 0); err != nil || n != 0 {
		t.Fatalf("early sweep: %d %v", n, err)
	}
	clk.Add(time.Minute)
	if _, err := s.Settle(ctx, "t1", "late", r1.Reservation.ID, 30, ""); code(err) != "reservation_expired" {
		t.Fatalf("settle after expiry: %v", err)
	}
	if err := s.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetReservation(ctx, "t1", r1.Reservation.ID)
	if got.Status != billing.StatusExpired {
		t.Fatalf("r1 status %s", got.Status)
	}
	if got, _ := s.GetReservation(ctx, "t1", r2.Reservation.ID); got.Status != billing.StatusReconciling {
		t.Fatalf("reconciling reservation was expired: %s", got.Status)
	}
	if got, _ := s.GetReservation(ctx, "t1", r3.Reservation.ID); got.Status != billing.StatusReserved {
		t.Fatalf("r3 status %s", got.Status)
	}
	a, _ := s.GetAccount(ctx, "t1", "a")
	if a.Available != 70 || a.Reserved != 30 {
		t.Fatalf("account after sweep: %+v", a)
	}
	if n, _ := s.ExpireReservations(ctx, 0); n != 0 {
		t.Fatalf("second sweep expired %d", n)
	}
	if _, err := s.Release(ctx, "t1", "rel", r1.Reservation.ID, ""); code(err) != "invalid_state" {
		t.Fatalf("release expired: %v", err)
	}
	assertBooks(t, s)
}

func TestReconcileDetectsTamperingAndLedgerIsAppendOnly(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	setup(t, s, "t1", "a", 100)
	must(t)(s.Reserve(ctx, "t1", "r", "a", 10, 0, ""))
	assertBooks(t, s)

	if _, err := s.DB.Write.ExecContext(ctx, `UPDATE billing_ledger_entries SET amount=1`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("ledger update: %v", err)
	}
	if _, err := s.DB.Write.ExecContext(ctx, `DELETE FROM billing_ledger_entries`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("ledger delete: %v", err)
	}
	if _, err := s.DB.Write.ExecContext(ctx, `UPDATE billing_accounts SET available=-1`); err == nil {
		t.Fatal("negative balance accepted")
	}
	if _, err := s.DB.Write.ExecContext(ctx, `UPDATE billing_accounts SET available=available+5, reserved=reserved-1 WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	ms, err := s.Reconcile(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]billing.Mismatch{}
	for _, m := range ms {
		fields[m.Field] = m
	}
	if len(ms) != 3 || fields["available"].Stored != 95 || fields["available"].Expected != 90 ||
		fields["reserved"].Expected != 10 || fields["open_reservations"].Expected != 10 {
		t.Fatalf("mismatches: %+v", ms)
	}
	if ms, _ := s.Reconcile(ctx, "other"); len(ms) != 0 {
		t.Fatalf("other tenant: %+v", ms)
	}
}

func TestValidation(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	setup(t, s, "t1", "a", 0)
	cases := []error{
		func() error { _, err := s.CreateAccount(ctx, "t1", "c1", "bad id", "USD"); return err }(),
		func() error { _, err := s.CreateAccount(ctx, "t1", "c2", "", "usd"); return err }(),
		func() error { _, err := s.Credit(ctx, "t1", "c3", "a", 0, ""); return err }(),
		func() error { _, err := s.Credit(ctx, "t1", "c4", "a", -5, ""); return err }(),
		func() error { _, err := s.Credit(ctx, "t1", "c5", "a", billing.MaxAmount+1, ""); return err }(),
		func() error { _, err := s.Reserve(ctx, "t1", "c6", "a", 1, 8*24*time.Hour, ""); return err }(),
		func() error { _, err := s.Credit(ctx, "t1", "", "a", 1, ""); return err }(),
		func() error { _, err := s.Credit(ctx, "", "c7", "a", 1, ""); return err }(),
	}
	for i, err := range cases {
		if code(err) != "invalid_request" {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if _, err := s.CreateAccount(ctx, "t1", "c8", "a", "CREDIT"); code(err) != "account_exists" {
		t.Fatalf("duplicate account: %v", err)
	}
	must(t)(s.Credit(ctx, "t1", "max", "a", billing.MaxAmount, ""))
	if _, err := s.Credit(ctx, "t1", "over", "a", 1, ""); code(err) != "balance_limit" {
		t.Fatalf("overflow: %v", err)
	}
	gen := must(t)(s.CreateAccount(ctx, "t1", "gen", "", "EUR"))
	if !strings.HasPrefix(gen.Account.ID, "acct_") {
		t.Fatalf("generated id %q", gen.Account.ID)
	}
}

// HTTP: API key auth, scope, tenant isolation, Idempotency-Key handling.
func TestHTTP(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	keys := auth.APIKeys{DB: s.DB}
	_, k1, err := keys.Create(ctx, "t1", "svc", []string{"billing"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, k2, _ := keys.Create(ctx, "t2", "svc", []string{"billing"}, 0)
	_, kNo, _ := keys.Create(ctx, "t1", "admin", []string{"admin"}, 0)
	mux := http.NewServeMux()
	s.Handler(mux, keys)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	do := func(method, path, key, idem, body string) (int, string, http.Header) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	expect := func(wantStatus int, wantSub string, st int, body string) {
		t.Helper()
		if st != wantStatus || !strings.Contains(body, wantSub) {
			t.Fatalf("got %d %s, want %d containing %q", st, body, wantStatus, wantSub)
		}
	}

	st, b, _ := do("POST", "/v1/billing/accounts", "", "c", `{"id":"u1","currency":"GEM"}`)
	expect(401, "unauthorized", st, b)
	st, b, _ = do("POST", "/v1/billing/accounts", kNo, "c", `{"id":"u1","currency":"GEM"}`)
	expect(403, "forbidden", st, b)
	st, b, _ = do("POST", "/v1/billing/accounts", k1, "", `{"id":"u1","currency":"GEM"}`)
	expect(400, "Idempotency-Key", st, b)
	st, b, _ = do("POST", "/v1/billing/accounts", k1, "c", `{"id":"u1","currency":"GEM","tenant_id":"t2"}`)
	expect(400, "invalid_request", st, b)
	st, b, _ = do("POST", "/v1/billing/accounts", k1, "c", `{"id":"u1","currency":"GEM"}`)
	expect(201, `"tenant_id":"t1"`, st, b)
	st, b, _ = do("POST", "/v1/billing/accounts/u1/credit", k1, "top", `{"amount":100}`)
	expect(200, `"available":100`, st, b)
	st, b, h := do("POST", "/v1/billing/accounts/u1/credit", k1, "top", `{"amount":100}`)
	expect(200, `"available":100`, st, b)
	if h.Get("Idempotent-Replayed") != "true" {
		t.Fatal("missing replay header")
	}
	st, b, _ = do("POST", "/v1/billing/accounts/u1/credit", k1, "top", `{"amount":5}`)
	expect(409, "idempotency_key_reused", st, b)
	st, b, _ = do("POST", "/v1/billing/accounts/u1/debit", k1, "d1", `{"amount":1000}`)
	expect(409, "insufficient_funds", st, b)
	st, b, _ = do("POST", "/v1/billing/accounts/u1/reservations", k1, "r1", `{"amount":40,"ttl_seconds":60,"note":"job"}`)
	expect(201, `"status":"reserved"`, st, b)
	rid := b[strings.Index(b, `"id":"rsv_`)+6:]
	rid = rid[:strings.Index(rid, `"`)]
	st, b, _ = do("POST", "/v1/billing/reservations/"+rid+"/uncertain", k1, "u1", `{"note":"timeout"}`)
	expect(200, `"status":"reconciling"`, st, b)
	st, b, _ = do("POST", "/v1/billing/reservations/"+rid+"/resolve", k1, "v1", `{"action":"settle","amount":15,"note":"confirmed"}`)
	expect(200, `"settled_amount":15`, st, b)
	st, b, _ = do("GET", "/v1/billing/accounts/u1", k1, "", "")
	expect(200, `"available":85`, st, b)
	st, b, _ = do("GET", "/v1/billing/accounts/u1/entries?limit=2", k1, "", "")
	expect(200, `"type":"settle"`, st, b)
	st, b, _ = do("GET", "/v1/billing/reservations/"+rid, k1, "", "")
	expect(200, `"status":"settled"`, st, b)
	st, b, _ = do("GET", "/v1/billing/reconcile", k1, "", "")
	expect(200, `"ok":true`, st, b)

	// Tenant isolation: t2's key cannot see or touch t1's account or reservation.
	st, b, _ = do("GET", "/v1/billing/accounts/u1", k2, "", "")
	expect(404, "not_found", st, b)
	st, b, _ = do("POST", "/v1/billing/accounts/u1/debit", k2, "x", `{"amount":1}`)
	expect(404, "not_found", st, b)
	st, b, _ = do("GET", "/v1/billing/reservations/"+rid, k2, "", "")
	expect(404, "not_found", st, b)
}
