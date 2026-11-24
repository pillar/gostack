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

func TestIPKey(t *testing.T) {
	if IPKey("2001:db8:1:2:aaaa::1") != IPKey("2001:db8:1:2:bbbb::9") || IPKey("2001:db8:1:3::1") == IPKey("2001:db8:1:2::1") {
		t.Fatal("ipv6 not aggregated to /64")
	}
	if IPKey("203.0.113.5") != "203.0.113.5" {
		t.Fatal("ipv4 changed")
	}
}
