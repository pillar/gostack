package commerce_test

import (
	"testing"
)

// Promo-code (purchaseType 1) and rewarded (2) purchases are not made through
// the standard billing flow and may come without an orderId; the purchase
// token identifies them. A standard purchase without orderId is still refused.
func TestGooglePromoPurchaseWithoutOrderID(t *testing.T) {
	f := setupGoogle(t)
	p := f.Guest("alpha")
	for i, typ := range []int{1, 2} {
		tok := []string{"promo-1", "rewarded-1"}[i]
		f.g.purchase(tok, "gems_100", f.order("alpha", p), func(m map[string]any) { m["purchaseType"] = typ; delete(m, "orderId") })
		r := f.Expect(f.verify("alpha", p, "gems_100", tok), 200)
		if r.Body["order"].(map[string]any)["state"] != "delivered" {
			t.Fatalf("%s", r.Raw)
		}
	}
	if f.gems("alpha", p) != 200 || f.runFinishJobs() != 2 {
		t.Fatal("promo purchases not delivered and finished")
	}
	f.g.purchase("std-1", "gems_100", f.order("alpha", p), func(m map[string]any) { delete(m, "orderId") })
	wantCode(t, f.verify("alpha", p, "gems_100", "std-1"), 400, "invalid_purchase")
	// A pending purchase has no orderId yet: it is reported as pending.
	f.g.purchase("pend-1", "gems_100", f.order("alpha", p), func(m map[string]any) { m["purchaseState"] = 2; delete(m, "orderId") })
	wantCode(t, f.verify("alpha", p, "gems_100", "pend-1"), 409, "purchase_pending")
}
