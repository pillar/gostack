package scale_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/modules/jobs"
)

const migDoc = `{
  "items": {"coin": {"kind": "currency"}, "gem": {"kind": "currency"}},
  "rewards": {"starter": {"grants": {"coin": 50}, "period": "once", "per_period": 1}},
  "offers": {"pack": {"cost": {"coin": 10}, "grants": {"gem": 1}, "period": "unlimited"}},
  "boards": {"main": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 1000, "quarantine_above": 900}}
}`

func modulesOn(c *config.Config) {
	c.Game.Modules = []string{"ops", "experiments", "social", "match", "commerce"}
	c.Billing.Enabled = true
	c.Auth.Web.Enabled = true
	c.Auth.Web.IPRatePerMinute, c.Auth.Web.AccountRatePerMinute = 1000, 1000
}

func cliRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := app.Main(args, app.Hooks{}, app.Env{Stdout: &out, Stderr: &errb, Getenv: func(string) (string, bool) { return "", false }})
	return code, out.String(), errb.String()
}

// PRD §12: data written through the HTTP API on SQLite is copied by
// `migrate to-postgres`, verified, and served unchanged by an app on PostgreSQL.
func TestMigrateSQLiteToPostgresEndToEnd(t *testing.T) {
	pgtest.RequirePostgres(t)
	src := gametest.New(t, gametest.Options{Config: migDoc, SQLite: true, Mutate: modulesOn})
	ctx := context.Background()
	_, billingKey, err := (auth.APIKeys{DB: src.App.DB}).Create(ctx, "default", "billing", []string{"billing"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	idem := func(k string) []string { return []string{"Idempotency-Key", k, "Content-Type", "application/json"} }
	p, q := src.Guest("alpha"), src.Guest("alpha")
	b := src.Guest("beta")
	src.Expect(src.Do("PUT", "/v1/games/alpha/saves/main", p.Token, map[string]any{"expected_revision": 0, "schema_version": 1, "payload": map[string]any{"lvl": 3, "name": "ü€"}}), 200)
	src.Expect(src.Do("PUT", "/v1/games/alpha/saves/main", p.Token, map[string]any{"expected_revision": 1, "schema_version": 1, "payload": map[string]any{"lvl": 4}}), 200)
	src.Expect(src.Do("POST", "/v1/games/alpha/rewards/starter/claim", p.Token, "{}", idem("r1")...), 200)
	src.Expect(src.Do("POST", "/v1/games/alpha/offers/pack/purchase", p.Token, "{}", idem("o1")...), 200)
	src.Expect(src.Do("POST", "/v1/games/alpha/boards/main/scores", p.Token, map[string]int{"score": 500}, idem("s1")...), 200)
	src.Expect(src.Do("POST", "/v1/games/alpha/boards/main/scores", q.Token, map[string]int{"score": 950}), 200) // quarantined (identity id)
	src.Expect(src.Do("POST", "/v1/games/beta/boards/main/scores", b.Token, map[string]int{"score": 1}), 200)
	src.Expect(src.Do("POST", "/v1/admin/games/alpha/mail", src.AdminToken, map[string]any{"player_id": p.ID, "title": "Hi", "body": "x", "grants": map[string]int{"gem": 2}}), 201)
	src.Expect(src.Do("POST", "/v1/games/alpha/friends/requests", p.Token, map[string]string{"player_id": q.ID}), 201)
	src.Expect(src.Do("POST", "/v1/games/alpha/friends/requests/"+p.ID+"/accept", q.Token, nil), 200)
	src.Expect(src.Do("PUT", "/v1/admin/games/alpha/match/queues/duel", src.AdminToken, `{"size":2,"rating_window":50,"widen_per_sec":10,"ttl":600}`), 200)
	src.Expect(src.Do("POST", "/v1/games/alpha/match/tickets", p.Token, map[string]any{"queue": "duel", "attributes": map[string]int{"rating": 1000}}), 201)
	src.Expect(src.Do("POST", "/v1/billing/accounts", billingKey, map[string]string{"id": "u1", "currency": "USD"}, idem("b1")...), 201)
	for i := range 3 {
		src.Expect(src.Do("POST", "/v1/billing/accounts/u1/credit", billingKey, map[string]int{"amount": 100 + i}, idem(fmt.Sprintf("c%d", i))...), 200)
	}
	src.Expect(src.Do("POST", "/v1/auth/signup", "", map[string]string{"email": "a@example.com", "password": "correct horse battery"}, "Content-Type", "application/json", "Origin", src.URL), 201)
	src.Expect(src.Do("POST", "/v1/games/alpha/events/batch", p.Token, map[string]any{"events": []any{map[string]any{"event_id": "e1",
		"event_name": "level_end", "session_id": "s1", "client_time": 1, "client_version": "1.0.0", "platform": "web", "schema_version": 1,
		"properties": map[string]int{"level": 3}}}}), 202)
	if _, err := src.App.Jobs.EnqueueNow(ctx, jobs.Job{Type: "mail", Payload: []byte{0, 1, 2, 255}, DedupeKey: "d1"}); err != nil {
		t.Fatal(err)
	}

	// Responses that must be identical after the move.
	reads := []struct{ path, token string }{
		{"/v1/games/alpha/me", p.Token},
		{"/v1/games/alpha/saves/main", p.Token},
		{"/v1/games/alpha/saves/main/revisions", p.Token},
		{"/v1/games/alpha/wallet", p.Token},
		{"/v1/games/alpha/inventory", p.Token},
		{"/v1/games/alpha/boards/main", p.Token},
		{"/v1/games/alpha/mail", p.Token},
		{"/v1/games/alpha/friends", q.Token},
		{"/v1/billing/accounts/u1", billingKey},
		{"/v1/billing/accounts/u1/entries", billingKey},
		{"/v1/admin/games/alpha/quarantine", src.AdminToken},
	}
	before := map[string]string{}
	for _, r := range reads {
		res := src.Do("GET", r.path, r.token, nil)
		if res.Status != 200 {
			t.Fatalf("GET %s on sqlite: %d %s", r.path, res.Status, res.Raw)
		}
		before[r.path] = string(res.Raw)
	}
	replay := src.Expect(src.Do("POST", "/v1/games/alpha/boards/main/scores", p.Token, map[string]int{"score": 500}, idem("s1")...), 200)

	// Refused while the source server holds the instance lock.
	dsnRef := pgtest.DSNRef(t, pgtest.NewDatabase(t))
	args := []string{"migrate", "to-postgres", "--from", src.Cfg.Database.Path, "--to-dsn-ref", dsnRef, "--analytics-from", src.Cfg.Database.AnalyticsPath}
	if code, _, errs := cliRun(t, args...); code == 0 || !strings.Contains(errs, "locked") {
		t.Fatalf("migration with the server running: code %d %s", code, errs)
	}
	src.Stop()

	code, out, errs := cliRun(t, args...)
	if code != 0 {
		t.Fatalf("migrate to-postgres: code %d\n%s\n%s", code, out, errs)
	}
	var rep app.ToPostgresReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK || !rep.Committed || len(rep.Tables) < 50 || len(rep.AnalyticsTables) != 1 || rep.AnalyticsTables[0].SourceRows != 1 {
		t.Fatalf("report: %s", out)
	}
	rows := map[string]int64{}
	for _, tc := range rep.Tables {
		if !tc.OK || tc.SourceRows != tc.TargetRows || tc.SourceDigest != tc.TargetDigest {
			t.Fatalf("table %+v", tc)
		}
		rows[tc.Table] = tc.SourceRows
	}
	for tbl, min := range map[string]int64{"players": 3, "sessions": 3, "save_revisions": 2, "game_ledger": 3, "score_quarantine": 1,
		"idempotency_records": 3, "jobs": 1, "ops_mail": 1, "friendships": 2, "match_tickets": 1, "billing_ledger_entries": 3,
		"billing_idempotency": 4, "web_users": 1, "api_keys": 2} {
		if rows[tbl] < min {
			t.Errorf("table %s copied %d rows, want >= %d", tbl, rows[tbl], min)
		}
	}
	// A second run refuses the now non-empty target.
	if code, _, errs := cliRun(t, args...); code == 0 || !strings.Contains(errs, "not empty") {
		t.Fatalf("second migration: code %d %s", code, errs)
	}

	// Serve the migrated data from PostgreSQL.
	cfg := src.Cfg
	cfg.Profile, cfg.Database.Driver, cfg.Database.DSNRef, cfg.Queue.Driver = "scale", "postgres", dsnRef, "postgres"
	cfg.RateLimit.AcceptPerInstance = true
	if code, out, errs := cliRunCfg(t, cfg, "migrate", "status"); code != 0 {
		t.Fatalf("migrate status on postgres: %d %s %s", code, out, errs)
	}
	dst := gametest.Attach(t, cfg, gametest.Options{}, src.AdminToken)
	for _, r := range reads {
		res := dst.Do("GET", r.path, r.token, nil)
		if res.Status != 200 || string(res.Raw) != before[r.path] {
			t.Errorf("GET %s after migration: %d\n got %s\nwant %s", r.path, res.Status, res.Raw, before[r.path])
		}
	}
	again := dst.Expect(dst.Do("POST", "/v1/games/alpha/boards/main/scores", p.Token, map[string]int{"score": 500}, idem("s1")...), 200)
	if again.Header.Get("Idempotent-Replayed") != "true" || string(again.Raw) != string(replay.Raw) {
		t.Fatalf("idempotent replay after migration: %s vs %s", again.Raw, replay.Raw)
	}
	// Identity sequences continue after the copied ids; writes keep working.
	dst.Expect(dst.Do("POST", "/v1/games/alpha/boards/main/scores", p.Token, map[string]int{"score": 990}), 200)
	dst.Expect(dst.Do("POST", "/v1/games/alpha/rewards/starter/claim", q.Token, "{}"), 200)
	var dup int
	if err := dst.App.DB.Read.QueryRow(`SELECT count(*) FROM jobs WHERE dedupe_key='d1' AND payload=?`, []byte{0, 1, 2, 255}).Scan(&dup); err != nil || dup != 1 {
		t.Fatalf("job payload bytes not preserved: %d %v", dup, err)
	}
	if code, out, errs := cliRunCfg(t, cfg, "game", "reconcile"); code != 0 {
		t.Fatalf("game reconcile on postgres: %d %s %s", code, out, errs)
	}
	// backup snapshot/verify on PostgreSQL use pg_dump/pg_restore (app/backup_pg.go).
	snap := t.TempDir() + "/x.dump"
	if code, _, errs := cliRunCfg(t, cfg, "backup", "snapshot", "--to", snap); code != 0 {
		if !strings.Contains(errs, "pg_dump not found") {
			t.Fatalf("backup snapshot on postgres: %d %s", code, errs)
		}
	} else if code, out, errs := cliRunCfg(t, cfg, "backup", "verify", snap); code != 0 {
		t.Fatalf("backup verify of the migrated database: %d %s %s", code, out, errs)
	}
	if code, out, errs := cliRunCfg(t, cfg, "doctor"); code != 0 || !strings.Contains(out, "postgres") {
		t.Fatalf("doctor on postgres: %d %s %s", code, out, errs)
	}
}

// cliRunCfg runs an app subcommand with cfg passed through --set overrides.
func cliRunCfg(t *testing.T, cfg config.Config, args ...string) (int, string, string) {
	t.Helper()
	set := []string{
		"--set", "profile=" + cfg.Profile, "--set", "database.driver=" + cfg.Database.Driver,
		"--set", "database.dsn_ref=" + cfg.Database.DSNRef, "--set", "queue.driver=" + cfg.Queue.Driver,
		"--set", "rate_limit.accept_per_instance=true",
	}
	return cliRun(t, append(set, args...)...)
}
