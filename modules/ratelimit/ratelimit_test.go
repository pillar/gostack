package ratelimit

import (
	"testing"
	"time"
)

func TestBucketAndBoundedKeys(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1, 2, 2)
	l.now = func() time.Time { return now }
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal()
	}
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal()
	}
	ok, retry := l.Allow("a")
	if ok || retry <= 0 || retry > time.Second {
		t.Fatalf("ok=%v retry=%v", ok, retry)
	}
	l.Allow("b")
	// Table full of active keys: a new client is still served and memory stays bounded.
	if ok, _ := l.Allow("c"); !ok {
		t.Fatal("new key denied when table full")
	}
	if len(l.buckets) > 2 {
		t.Fatalf("keys=%d exceeds bound", len(l.buckets))
	}
}

func TestSetPerMinute(t *testing.T) {
	now := time.Unix(0, 0)
	l := PerMinute(10, 100)
	l.now = func() time.Time { return now }
	l.Allow("a")
	l.SetPerMinute(2) // lowering caps existing buckets immediately
	n := 0
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("a"); ok {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("after lowering to 2/min allowed %d", n)
	}
	l.SetPerMinute(5) // raising credits the +3 burst immediately, without waiting
	n = 0
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("a"); ok {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("after raising 2->5/min allowed %d, want 3", n)
	}
	l.SetPerMinute(120) // 2/s refill
	for ok := true; ok; ok, _ = l.Allow("a") {
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("raised rate did not refill")
	}
	if ok, _ := l.Allow("fresh"); !ok {
		t.Fatal("new key denied")
	}
	l.SetPerMinute(0)
	if l.burst != 120 {
		t.Fatal("invalid rate applied")
	}
}

func TestIPKey(t *testing.T) {
	if IPKey("2001:db8:1:2:aaaa::1") != IPKey("2001:db8:1:2:bbbb::9") || IPKey("2001:db8:1:3::1") == IPKey("2001:db8:1:2::1") {
		t.Fatal("ipv6 not aggregated to /64")
	}
	if IPKey("203.0.113.5") != "203.0.113.5" {
		t.Fatal("ipv4 changed")
	}
}
