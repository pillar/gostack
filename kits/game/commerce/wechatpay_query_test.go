package commerce_test

import (
	"context"
	"slices"
	"testing"
	"time"
)

// A WeChat Pay order whose payment notification never arrives is found by the
// periodic order query (Module.Maintain) and delivered through the same path
// as the notification, exactly once.
func TestWeChatPayQueryReconciliation(t *testing.T) {
	f := setupWeChat(t)
	ctx := context.Background()
	mod := f.App.Mod.Commerce
	clock := &testClock{t: time.Now()}
	mod.Now = clock.Now
	p := f.wxPlayer("alpha", "o-alpha")
	paidID, paidOTN := f.prepaid("alpha", p)
	_, unpaidOTN := f.prepaid("alpha", p)
	_, unknownOTN := f.prepaid("alpha", p)
	f.w.mu.Lock()
	f.w.orders = map[string]map[string]any{paidOTN: paid(paidOTN, "4200000555", 600)}
	unpaid := paid(unpaidOTN, "", 600)
	unpaid["trade_state"] = "NOTPAY"
	delete(unpaid, "transaction_id")
	f.w.orders[unpaidOTN] = unpaid
	f.w.mu.Unlock()

	// Younger than query_after (default 10m): not queried.
	if err := mod.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if q := f.w.queriedNos(); len(q) != 0 {
		t.Fatalf("queried %v", q)
	}
	clock.Set(clock.Now().Add(11 * time.Minute))
	_, freshOTN := f.prepaid("alpha", p) // created now: still too young
	if err := mod.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	q := f.w.queriedNos()
	slices.Sort(q)
	want := []string{paidOTN, unpaidOTN, unknownOTN}
	slices.Sort(want)
	if !slices.Equal(q, want) || slices.Contains(q, freshOTN) {
		t.Fatalf("queried %v want %v", q, want)
	}
	if f.gems("alpha", p) != 100 || f.count(`SELECT count(*) FROM game_ledger WHERE source_key='iap:wechatpay:4200000555'`) != 1 {
		t.Fatal("paid order not delivered by the query")
	}
	g := f.Expect(f.Do("GET", "/v1/games/alpha/iap/orders/"+paidID, p.Token, nil), 200).Body["order"].(map[string]any)
	if g["state"] != "delivered" || g["provider_txn_id"] != "4200000555" {
		t.Fatalf("%v", g)
	}
	// The late notification is a no-op; another query run does not query delivered orders.
	f.Expect(f.send("alpha", f.w.notification("evt-late", "TRANSACTION.SUCCESS", "transaction", paid(paidOTN, "4200000555", 600), f.w.apiv3)), 204)
	f.w.mu.Lock()
	f.w.queried = nil
	f.w.mu.Unlock()
	if err := mod.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if q := f.w.queriedNos(); f.gems("alpha", p) != 100 || slices.Contains(q, paidOTN) {
		t.Fatalf("gems=%d queried=%v", f.gems("alpha", p), q)
	}
	// A response whose signature does not verify delivers nothing.
	f.w.mu.Lock()
	f.w.orders[unpaidOTN] = paid(unpaidOTN, "4200000556", 600)
	f.w.badQuerySig = true
	f.w.mu.Unlock()
	if err := mod.Maintain(ctx); err == nil {
		t.Fatal("expected a reconciliation error for the bad response signature")
	}
	if f.gems("alpha", p) != 100 {
		t.Fatal("delivered from an unverified response")
	}
	// Past the prepay expiry (2h) plus margin the order is no longer queried.
	clock.Set(clock.Now().Add(4 * time.Hour))
	f.w.mu.Lock()
	f.w.badQuerySig, f.w.queried = false, nil
	f.w.mu.Unlock()
	if err := mod.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if q := f.w.queriedNos(); len(q) != 0 || f.gems("alpha", p) != 100 {
		t.Fatalf("queried %v", q)
	}
}

func (w *wxFake) queriedNos() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.queried)
}
