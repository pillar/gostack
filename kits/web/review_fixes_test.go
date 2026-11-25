package web_test

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/kits/web"
	"github.com/pillar/gostack/modules/billing"
)

// deniedWebhookIPs covers every special-purpose class the SSRF guard refuses.
var deniedWebhookIPs = []string{
	"0.1.2.3",         // 0.0.0.0/8 "this network"
	"10.1.2.3",        // RFC 1918
	"100.64.0.1",      // 100.64.0.0/10 CGNAT
	"100.100.100.200", // Alibaba Cloud metadata (inside 100.64.0.0/10)
	"127.0.0.1",       // loopback
	"169.254.169.254", // link-local / cloud metadata
	"172.16.0.1",      // RFC 1918
	"192.0.0.8",       // IETF protocol assignments
	"192.0.2.1",       // TEST-NET-1
	"192.168.1.1",     // RFC 1918
	"198.18.0.1",      // benchmarking
	"198.51.100.1",    // TEST-NET-2
	"203.0.113.1",     // TEST-NET-3
	"224.0.0.1",       // multicast
	"240.0.0.1",       // reserved
	"255.255.255.255", // broadcast
	"::",              // unspecified
	"::1",             // loopback
	"::ffff:127.0.0.1",
	"::ffff:100.100.100.200",
	"64:ff9b::a9fe:a9fe",   // NAT64 well-known prefix → 169.254.169.254
	"64:ff9b::7f00:1",      // NAT64 → 127.0.0.1
	"64:ff9b:1::a9fe:a9fe", // NAT64 local-use prefix
	"2002:a9fe:a9fe::1",    // 6to4 → 169.254.169.254
	"fc00::1",              // unique local
	"fd12:3456::1",         // unique local
	"fe80::1",              // link-local
	"fec0::1",              // site-local (deprecated)
	"ff02::1",              // multicast
	"100::1",               // discard-only
	"2001:db8::1",          // documentation
	"2001::1",              // Teredo (2001::/23 special)
	"2001:10::1",           // ORCHID (2001::/23 special)
}

// The dial-time guard refuses every special-purpose address class, not only
// loopback/RFC 1918 (CGNAT 100.64/10 reaches Alibaba Cloud's metadata service,
// NAT64 and 6to4 embed IPv4 targets).
func TestWebhookDialGuardDeniesSpecialRanges(t *testing.T) {
	c := web.NewWebhookClient(false)
	for _, ip := range deniedWebhookIPs {
		t.Run(ip, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(ip, "80")+"/", nil)
			_, err := c.Do(req)
			if err == nil || !strings.Contains(err.Error(), "not a public address") {
				t.Fatalf("%s: dial was not refused by the guard: %v", ip, err)
			}
		})
	}
}

// Literal IP URLs are refused when the webhook is created.
func TestCreateWebhookRefusesNonPublicLiteralIPs(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	for _, ip := range deniedWebhookIPs {
		u := "https://" + net.JoinHostPort(ip, "443") + "/hook"
		if _, err := e.k.CreateWebhook(ctx, "ta", u, nil); code(err) != "invalid_request" {
			t.Errorf("%s accepted at creation: %v", u, err)
		}
	}
	for _, u := range []string{"https://8.8.8.8/hook", "https://[2606:4700:4700::1111]/hook", "https://example.com/hook"} {
		if _, err := e.k.CreateWebhook(ctx, "ta", u, nil); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	// webhook_allow_private_networks (development) turns the check off.
	e.k.Cfg.WebhookAllowPrivateNetworks = true
	if _, err := e.k.CreateWebhook(ctx, "tb", "http://127.0.0.1:9/hook", nil); err != nil {
		t.Fatalf("allow private: %v", err)
	}
}

// The default delivery client is built once per Kit: deliveries reuse its
// connection pool instead of leaking a transport (and its idle connection
// goroutines) per delivery.
func TestWebhookClientReusedAcrossDeliveries(t *testing.T) {
	e := newEnv(t, false)
	e.k.Client = nil
	e.k.Cfg.WebhookAllowPrivateNetworks = true // the receiver listens on loopback
	ctx := context.Background()
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer recv.Close()
	_, err := e.k.CreateWebhook(ctx, "ta", recv.URL+"/hook", []string{"order.created"})
	must(t, err)
	e.account(t, "ta", "wallet", 1000)
	deliver := func(n int) {
		for i := 0; i < n; i++ {
			createOrder(t, e, "ta", "wallet", 1)
		}
		if errs := e.runJobs(t); len(errs) != 0 {
			t.Fatalf("deliveries failed: %v", errs)
		}
	}
	deliver(5) // warm up: one pooled connection
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	deliver(40)
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	t.Logf("goroutines before %d, after 40 deliveries %d", before, after)
	if after-before > 10 {
		t.Fatalf("goroutines grew from %d to %d over 40 deliveries (transport per delivery?)", before, after)
	}
}

func TestPublicIP(t *testing.T) {
	for _, s := range deniedWebhookIPs {
		if web.PublicIP(netip.MustParseAddr(s)) {
			t.Errorf("%s treated as public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "100.63.255.255", "100.128.0.1", "172.32.0.1", "198.20.0.1", "223.255.255.255",
		"2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8", "64:ff9b::808:808", "2002:808:808::1"} {
		if !web.PublicIP(netip.MustParseAddr(s)) {
			t.Errorf("%s treated as non-public", s)
		}
	}
}

// A Pay that stalls past the reservation TTL after reserving must not leave
// the order stuck: retrying it notices the expired reservation and reserves
// again under a new payment generation.
func TestPayRecoversFromExpiredReservation(t *testing.T) {
	for _, swept := range []bool{false, true} {
		e := newEnv(t, false)
		ctx := context.Background()
		e.account(t, "ta", "wallet", 1000)
		o := createOrder(t, e, "ta", "wallet", 400)
		// First half of Pay (reserve with the order's key, record it), then the process stalls.
		res, err := e.b.Reserve(ctx, "ta", "web-order:"+o.ID+":reserve", "wallet", 400, 0, "web order "+o.ID)
		must(t, err)
		must(t, e.k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE web_orders SET reservation_id=? WHERE tenant_id=? AND id=?`, res.Reservation.ID, "ta", o.ID)
			return err
		}))
		e.clk.Add(billing.DefaultReservationTTL + time.Minute)
		if swept {
			if _, err := e.b.ExpireReservations(ctx, 100); err != nil {
				t.Fatal(err)
			}
		}
		paid, err := e.k.Pay(ctx, "ta", o.ID)
		if err != nil {
			t.Fatalf("swept=%v: retried pay: %v", swept, err)
		}
		if paid.Status != web.OrderPaid || paid.ReservationID == res.Reservation.ID {
			t.Fatalf("swept=%v: order %+v", swept, paid)
		}
		a, _ := e.b.GetAccount(ctx, "ta", "wallet")
		if a.Available != 600 || a.Reserved != 0 {
			t.Fatalf("swept=%v: balance available %d reserved %d", swept, a.Available, a.Reserved)
		}
		if old, _ := e.b.GetReservation(ctx, "ta", res.Reservation.ID); old.Status == billing.StatusReserved || old.Status == billing.StatusSettled {
			t.Fatalf("swept=%v: old reservation %s", swept, old.Status)
		}
		// Paying again is a no-op; nothing is charged twice.
		if again, err := e.k.Pay(ctx, "ta", o.ID); err != nil || again.Status != web.OrderPaid {
			t.Fatalf("swept=%v: repeat pay %v %v", swept, again.Status, err)
		}
		if b := e.balance(t, "ta", "wallet"); b != 600 {
			t.Fatalf("swept=%v: charged twice: %d", swept, b)
		}
	}
}

// Publish(rev=0) must publish the draft that is current when its transaction
// commits. On PostgreSQL a concurrent edit forces a serialization failure and
// WriteTx re-runs the closure; it must not reuse the revision resolved by the
// failed attempt. (SQLite has a single writer, so the race cannot occur there.)
func TestPublishCurrentDraftIsResolvedPerAttempt(t *testing.T) {
	if !pgtest.Enabled() {
		t.Skip("needs PostgreSQL (GOSTACK_TEST_POSTGRES_DSN): concurrent writers")
	}
	e := newEnv(t, false)
	ctx := context.Background()
	_, err := e.k.CreateSite(ctx, "ta", web.Site{ID: "blog", Name: "Blog", BaseURL: "https://blog.example.com"})
	must(t, err)
	p, err := e.k.CreatePage(ctx, "ta", "blog", "u", web.PageInput{Slug: "a", Title: "A", Body: "v1"})
	must(t, err)
	var armed atomic.Bool
	e.k.Now = func() time.Time {
		// Inside Publish's transaction (after it read the page): another
		// editor saves revision 2 and commits.
		if armed.CompareAndSwap(true, false) {
			if _, err := e.k.UpdatePage(ctx, "ta", p.ID, "u", web.PageInput{Slug: "a", Title: "A", Body: "v2", ExpectedVersion: p.Version}); err != nil {
				t.Errorf("concurrent edit: %v", err)
			}
		}
		return e.clk.Now()
	}
	busy := e.k.DB.Stats().Busy
	armed.Store(true)
	got, err := e.k.Publish(ctx, "ta", p.ID, 0, time.Time{})
	must(t, err)
	if e.k.DB.Stats().Busy == busy {
		t.Fatal("the concurrent edit did not force a retry")
	}
	if got.DraftRevision != 2 || got.PublishedRevision != 2 {
		t.Fatalf("published revision %d of draft %d (stale revision from the failed attempt)", got.PublishedRevision, got.DraftRevision)
	}
}
