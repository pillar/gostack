package ops_test

import (
	"testing"
)

// A timezone change by the admin must not open a second check-in inside the
// same real day; a genuine local-midnight rollover still allows the next claim.
func TestCheckinTimezoneChangeNoDoubleClaim(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	put := func(tz string) {
		f.Expect(f.Do("PUT", admin("alpha", "/checkin"), f.AdminToken, obj("cycle", []any{obj("coin", 10)}, "timezone", tz, "streak_reset", true)), 200)
	}
	put("UTC")
	f.Clk.Set(utc("2026-11-25T20:00:00Z"))
	f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200)
	put("Pacific/Kiritimati") // UTC+14: it is already 2026-11-26 10:00 there
	st := f.Expect(f.Do("GET", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if st["checked_in_today"] != true {
		t.Fatalf("status after tz change: %v", st)
	}
	if c := f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 409).Code(); c != "already_checked_in" {
		t.Fatal(c)
	}
	if b := f.balance("alpha", p, "coin"); b != 10 {
		t.Fatalf("double claim after tz change: coin=%d", b)
	}
	// Next real local day in the new zone (2026-11-27 Kiritimati) works and the streak continues.
	f.Clk.Set(utc("2026-11-26T10:30:00Z"))
	r := f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if r["streak"] != 2.0 {
		t.Fatalf("%v", r)
	}
}

func TestCheckinLocalMidnightRollover(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	f.Expect(f.Do("PUT", admin("alpha", "/checkin"), f.AdminToken, obj("cycle", []any{obj("coin", 10)}, "timezone", "Asia/Shanghai", "streak_reset", true)), 200)
	f.Clk.Set(utc("2026-11-25T15:59:00Z")) // 23:59 Shanghai
	f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200)
	f.Clk.Set(utc("2026-11-25T16:01:00Z")) // 00:01 next Shanghai day
	r := f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if r["streak"] != 2.0 || r["day"] != "2026-11-26" {
		t.Fatalf("%v", r)
	}
	f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 409)
}

func TestTaskTimezoneChangeNoDoubleClaim(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	put := func(id, period, tz string) {
		f.Expect(f.Do("PUT", admin("alpha", "/tasks/"+id), f.AdminToken, obj("metric", "wins", "target", 1, "period", period, "grants", obj("coin", 10), "timezone", tz)), 200)
	}
	win := func() {
		f.Expect(f.Do("POST", admin("alpha", "/progress"), f.AdminToken, obj("player_id", p.ID, "metric", "wins", "delta", 1)), 200)
	}
	put("d", "daily", "UTC")
	put("w", "weekly", "UTC")
	// Sunday 2026-11-29 20:00Z is Monday 2026-11-30 10:00 (next ISO week) in Kiritimati.
	f.Clk.Set(utc("2026-11-29T20:00:00Z"))
	win()
	f.Expect(f.Do("POST", pub("alpha", "/tasks/d/claim"), p.Token, nil), 200)
	f.Expect(f.Do("POST", pub("alpha", "/tasks/w/claim"), p.Token, nil), 200)
	put("d", "daily", "Pacific/Kiritimati")
	put("w", "weekly", "Pacific/Kiritimati")
	win()
	for _, id := range []string{"d", "w"} {
		if c := f.Expect(f.Do("POST", pub("alpha", "/tasks/"+id+"/claim"), p.Token, nil), 409).Code(); c != "already_claimed" {
			t.Fatalf("%s: %s", id, c)
		}
	}
	if b := f.balance("alpha", p, "coin"); b != 20 {
		t.Fatalf("double claim after tz change: coin=%d", b)
	}
	for _, x := range f.Expect(f.Do("GET", pub("alpha", "/tasks"), p.Token, nil), 200).Body["tasks"].([]any) {
		if m := x.(map[string]any); m["claimable"] != false || m["claimed"] != true {
			t.Fatalf("list after tz change: %v", m)
		}
	}
	// Next real local day in Kiritimati: the daily task is claimable again.
	f.Clk.Set(utc("2026-11-30T10:30:00Z"))
	win()
	f.Expect(f.Do("POST", pub("alpha", "/tasks/d/claim"), p.Token, nil), 200)
}

func TestTaskDailyLocalMidnightRollover(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	f.Expect(f.Do("PUT", admin("alpha", "/tasks/d"), f.AdminToken, obj("metric", "wins", "target", 1, "period", "daily", "grants", obj("coin", 10), "timezone", "UTC")), 200)
	win := func() {
		f.Expect(f.Do("POST", admin("alpha", "/progress"), f.AdminToken, obj("player_id", p.ID, "metric", "wins", "delta", 1)), 200)
	}
	f.Clk.Set(utc("2026-11-25T23:59:00Z"))
	win()
	f.Expect(f.Do("POST", pub("alpha", "/tasks/d/claim"), p.Token, nil), 200)
	f.Clk.Set(utc("2026-11-26T00:01:00Z"))
	win()
	f.Expect(f.Do("POST", pub("alpha", "/tasks/d/claim"), p.Token, nil), 200)
	f.Expect(f.Do("POST", pub("alpha", "/tasks/d/claim"), p.Token, nil), 409)
}
