package commerce_test

import (
	"testing"
	"time"
)

// Provider callbacks (server-to-server, authenticated by the provider's
// signature) are exempt from the per-game request quotas: unauthenticated junk
// that exhausts a game's requests_per_minute must not make a signed Google RTDN
// or WeChat Pay notification fail with 429.
func TestCallbacksExemptFromRequestQuota(t *testing.T) {
	t.Run("google", func(t *testing.T) {
		f := setupGoogle(t)
		f.Expect(f.Do("PUT", "/v1/admin/games/alpha/quotas", f.AdminToken, map[string]any{"requests_per_minute": 5}), 200)
		body := pushBody("junk", voided("zzz", 1))
		for range 20 {
			f.Do("POST", "/v1/games/alpha/callbacks/google", "not-a-jwt", body)
			f.Do("GET", "/v1/games/alpha/callbacks/ads/admob", "", nil)
			f.Do("GET", "/v1/games/alpha/config", "", nil)
		}
		// Ordinary public routes are still limited.
		wantCode(t, f.Do("GET", "/v1/games/alpha/config", "", nil), 429, "rate_limited")
		r := f.Expect(f.push(t, "alpha", "m-quota", voided("zzz", 1)), 200)
		if r.Body["status"] != "processed" {
			t.Fatalf("%s", r.Raw)
		}
	})
	t.Run("wechatpay", func(t *testing.T) {
		f := setupWeChat(t)
		p := f.wxPlayer("alpha", "o-alpha")
		_, otn := f.prepaid("alpha", p)
		f.Expect(f.Do("PUT", "/v1/admin/games/alpha/quotas", f.AdminToken, map[string]any{"requests_per_minute": 5}), 200)
		junk := f.w.notification("junk", "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200009999", 600), f.w.apiv3)
		for range 20 {
			f.notify("alpha", junk, nil) // unsigned
			f.Do("GET", "/v1/games/alpha/config", "", nil)
		}
		n := f.w.notification("evt-q", "TRANSACTION.SUCCESS", "transaction", paid(otn, "4200000777", 600), f.w.apiv3)
		if r := f.notify("alpha", n, f.w.sign(n, time.Now())); r.Status != 204 {
			t.Fatalf("%d %s", r.Status, r.Raw)
		}
		if f.count(`SELECT count(*) FROM game_ledger WHERE source_key='iap:wechatpay:4200000777'`) != 1 {
			t.Fatal("not delivered")
		}
	})
}
