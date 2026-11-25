package game_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
)

func (e *env) waitEvents(where string, want int) int {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		if err := e.a.Analytics.Read.QueryRow(`SELECT count(*) FROM analytics_events WHERE ` + where).Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		if n >= want {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return n
}

// Text that PostgreSQL TEXT cannot store (NUL, invalid UTF-8) and other control
// characters are rejected at ingress, so they never reach the shared flush batch.
func TestEventsBatchRejectsControlCharsAndInvalidUTF8(t *testing.T) {
	e := newEnv(t)
	p := e.guest("alpha")
	good := func(id string) map[string]any {
		return map[string]any{"event_id": id, "event_name": "level_end", "session_id": "s1", "client_time": 1, "client_version": "1.0.0", "platform": "web", "schema_version": 1, "properties": map[string]any{"name": "Ünïcode ✓"}}
	}
	e.expect(e.do("POST", "/v1/games/alpha/events/batch", p.token, map[string]any{"events": []any{good("g1"), good("g2")}}), 202)
	for name, body := range map[string]string{
		"nul platform":            `{"events":[{"event_id":"b1","event_name":"x","session_id":"s","platform":"a\u0000b"}]}`,
		"control client_version":  `{"events":[{"event_id":"b2","event_name":"x","session_id":"s","client_version":"1\u0001"}]}`,
		"del platform":            `{"events":[{"event_id":"b3","event_name":"x","session_id":"s","platform":"a` + "\x7f" + `"}]}`,
		"invalid utf8 properties": `{"events":[{"event_id":"b4","event_name":"x","session_id":"s","properties":{"k":"a` + "\xff" + `b"}}]}`,
		"invalid utf8 platform":   `{"events":[{"event_id":"b5","event_name":"x","session_id":"s","platform":"a` + "\xff" + `"}]}`,
	} {
		r := e.do("POST", "/v1/games/alpha/events/batch", "", body)
		if r.Status != 400 {
			t.Errorf("%s: status %d want 400: %s", name, r.Status, r.Raw)
		}
	}
	// An invalid event rejects the whole request: valid events before it are not ingested.
	e.expect(e.do("POST", "/v1/games/alpha/events/batch", "", `{"events":[{"event_id":"g4","event_name":"x","session_id":"s"},{"event_id":"b6","event_name":"x","session_id":"s","platform":"a\u0000"}]}`), 400)
	// A NUL escape inside properties stays an escape (6 ASCII bytes) and is accepted.
	e.expect(e.do("POST", "/v1/games/alpha/events/batch", "", `{"events":[{"event_id":"g3","event_name":"x","session_id":"s","properties":{"k":"a\u0000b"}}]}`), 202)
	if n := e.waitEvents(`game_id='alpha' AND event_id IN ('g1','g2','g3')`, 3); n != 3 {
		t.Fatalf("good events stored=%d want 3", n)
	}
	if n := e.waitEvents(`event_id='g4'`, 0); n != 0 {
		t.Fatal("event before the invalid one was ingested from a rejected request")
	}
	var props string
	if err := e.a.Analytics.Read.QueryRow(`SELECT properties FROM analytics_events WHERE game_id='alpha' AND event_id='g3'`).Scan(&props); err != nil {
		t.Fatal(err)
	}
	if props != `{"k":"a\u0000b"}` {
		t.Fatalf("properties stored as %q", props)
	}
}

// A row the database rejects drops only itself: the other events of the batch
// (other players') are still written and the bad one is counted as dropped.
func TestAnalyticsFlushIsolatesPoisonRow(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UnixMilli()
	an := e.a.Game.Analytics
	for i := 0; i < 5; i++ {
		an.OfferRawForTest("alpha", "", fmt.Sprintf("a%d", i), "web", now)
	}
	an.OfferRawForTest("alpha", "", "poison", "a\x00b", now)
	for i := 0; i < 5; i++ {
		an.OfferRawForTest("beta", "", fmt.Sprintf("b%d", i), "web", now)
	}
	if n := e.waitEvents(`event_id<>'poison'`, 10); n != 10 {
		t.Fatalf("good events stored=%d want 10", n)
	}
	if pgtest.Enabled() {
		var n int
		_ = e.a.Analytics.Read.QueryRow(`SELECT count(*) FROM analytics_events WHERE event_id='poison'`).Scan(&n)
		if _, dropped, written, _ := an.Stats(); n != 0 || dropped != 1 || written != 10 {
			t.Fatalf("poison stored=%d dropped=%d written=%d", n, dropped, written)
		}
	}
}

// The payload bytes of PUT /saves and restore responses are exactly the stored
// payload covered by the checksum, on the first response and on replay.
func TestSaveResponsePayloadMatchesChecksum(t *testing.T) {
	e := newEnv(t)
	p := e.guest("alpha")
	payload := "{\n  \"level\" :  3,\n\t\"name\": \"<a&b>\",\n  \"items\": [ 1, 2 ]\n}"
	sum := sha256.Sum256([]byte(payload))
	want := hex.EncodeToString(sum[:])
	body := `{"expected_revision":0,"schema_version":1,"payload":` + payload + `}`
	check := func(r resp, what string) {
		t.Helper()
		var out struct {
			Checksum string          `json:"checksum"`
			Payload  json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(r.Raw, &out); err != nil {
			t.Fatal(err)
		}
		got := sha256.Sum256(out.Payload)
		if out.Checksum != want || hex.EncodeToString(got[:]) != want {
			t.Errorf("%s: checksum %s, sha256(payload)=%x want %s; payload %q", what, out.Checksum, got, want, out.Payload)
		}
	}
	r := e.expect(e.do("PUT", "/v1/games/alpha/saves/main", p.token, body, "Idempotency-Key", "k1"), 200)
	check(r, "first put")
	r = e.expect(e.do("PUT", "/v1/games/alpha/saves/main", p.token, body, "Idempotency-Key", "k1"), 200)
	if r.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("expected replay")
	}
	check(r, "replayed put")
	e.expect(e.do("PUT", "/v1/games/alpha/saves/main", p.token, `{"expected_revision":1,"schema_version":1,"payload":{"x":1}}`), 200)
	restore := `{"revision":1,"expected_revision":2}`
	r = e.expect(e.do("POST", "/v1/games/alpha/saves/main/restore", p.token, restore, "Idempotency-Key", "k2"), 200)
	check(r, "restore")
	r = e.expect(e.do("POST", "/v1/games/alpha/saves/main/restore", p.token, restore, "Idempotency-Key", "k2"), 200)
	check(r, "replayed restore")
	r = e.expect(e.do("GET", "/v1/games/alpha/saves/main", p.token, nil), 200)
	check(r, "get")
}

// game_scores_quarantined_total counts committed quarantines only: not a
// transaction attempt that was rolled back (SQLite: the request fails after
// the quarantine insert) or re-run (PostgreSQL: one serialization failure).
func TestQuarantineMetricCountsCommittedOnly(t *testing.T) {
	e := newEnv(t)
	p := e.guest("alpha")
	ctx := context.Background()
	counter := e.a.Metrics.Counter("game_scores_quarantined_total", "")
	before := counter.Load()
	if pgtest.Enabled() {
		for _, q := range []string{
			`CREATE SEQUENCE test_fail_once`,
			`CREATE FUNCTION test_fail_once() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF nextval('test_fail_once') = 1 THEN RAISE EXCEPTION 'injected' USING ERRCODE = '40001'; END IF; RETURN NEW; END $$`,
			`CREATE TRIGGER test_fail_once BEFORE INSERT ON idempotency_records FOR EACH ROW EXECUTE FUNCTION test_fail_once()`,
		} {
			if _, err := e.a.DB.Write.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		busy := e.a.DB.Stats().Busy
		e.expect(e.do("POST", "/v1/games/alpha/boards/main/scores", p.token, `{"score":600000}`, "Idempotency-Key", "q1"), 200)
		if e.a.DB.Stats().Busy != busy+1 {
			t.Fatal("injected serialization failure did not re-run the transaction")
		}
	} else {
		if _, err := e.a.DB.Write.ExecContext(ctx, `CREATE TRIGGER test_fail BEFORE INSERT ON idempotency_records
			WHEN NEW.key='boom' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
			t.Fatal(err)
		}
		if r := e.do("POST", "/v1/games/alpha/boards/main/scores", p.token, `{"score":600000}`, "Idempotency-Key", "boom"); r.Status != 500 {
			t.Fatalf("status %d: %s", r.Status, r.Raw)
		}
		e.expect(e.do("POST", "/v1/games/alpha/boards/main/scores", p.token, `{"score":600000}`, "Idempotency-Key", "q1"), 200)
	}
	r := e.expect(e.do("POST", "/v1/games/alpha/boards/main/scores", p.token, `{"score":600000}`, "Idempotency-Key", "q1"), 200)
	if r.Header.Get("Idempotent-Replayed") != "true" || !strings.Contains(string(r.Raw), "quarantined") {
		t.Fatalf("replay %s", r.Raw)
	}
	var n int
	if err := e.a.DB.Read.QueryRow(`SELECT count(*) FROM score_quarantine WHERE game_id='alpha'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("quarantine rows %d %v", n, err)
	}
	if got := counter.Load() - before; got != 1 {
		t.Fatalf("game_scores_quarantined_total += %d, want 1", got)
	}
}
