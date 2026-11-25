package cache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/valkey"
	"github.com/pillar/gostack/adapters/valkeytest"
	"github.com/pillar/gostack/core/observability"
)

var ctx = context.Background()

func TestMemoryBoundsAndLRU(t *testing.T) {
	const maxBytes = 10_000
	m := NewMemory(maxBytes, 1000)
	val := []byte(strings.Repeat("v", 400))
	for i := range 100 {
		if err := m.Set(ctx, fmt.Sprintf("k%03d", i), val, time.Hour); err != nil {
			t.Fatal(err)
		}
		used, n, _ := m.Stats()
		if used > maxBytes {
			t.Fatalf("used %d > max %d after %d sets", used, maxBytes, i+1)
		}
		if want := int64(n) * (4 + 400 + entryOverhead); used != want {
			t.Fatalf("accounting %d want %d", used, want)
		}
	}
	_, n, ev := m.Stats()
	if n != maxBytes/(4+400+entryOverhead) || ev != int64(100-n) {
		t.Fatalf("entries %d evictions %d", n, ev)
	}
	// Oldest keys were evicted; the newest remain.
	if _, ok, _ := m.Get(ctx, "k000"); ok {
		t.Fatal("oldest key survived")
	}
	if _, ok, _ := m.Get(ctx, "k099"); !ok {
		t.Fatal("newest key evicted")
	}
	// Reading a key makes it most recently used.
	first := fmt.Sprintf("k%03d", 100-n)
	m.Get(ctx, first)
	m.Set(ctx, "new", val, time.Hour)
	if _, ok, _ := m.Get(ctx, first); !ok {
		t.Fatal("recently read key evicted before older ones")
	}
	// Overwrite does not double count.
	before, _, _ := m.Stats()
	m.Set(ctx, "new", val, time.Hour)
	if after, _, _ := m.Stats(); after != before {
		t.Fatalf("overwrite changed accounting %d -> %d", before, after)
	}
	if err := m.Set(ctx, "big", make([]byte, 1001), time.Hour); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("oversized value: %v", err)
	}
	if err := m.Set(ctx, "k", val, 0); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("zero ttl: %v", err)
	}
	if err := m.Set(ctx, "", val, time.Hour); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty key: %v", err)
	}
	m.Delete(ctx, "new")
	if _, ok, _ := m.Get(ctx, "new"); ok {
		t.Fatal("deleted key returned")
	}
}

func TestMemoryTTLAndCopies(t *testing.T) {
	now := time.Unix(1000, 0)
	m := NewMemory(1<<20, 0)
	m.now = func() time.Time { return now }
	buf := []byte("abc")
	m.Set(ctx, "k", buf, time.Second)
	buf[0] = 'X' // caller reuses its slice
	v, ok, _ := m.Get(ctx, "k")
	if !ok || string(v) != "abc" {
		t.Fatalf("%q %v", v, ok)
	}
	v[1] = 'Y'
	if v2, _, _ := m.Get(ctx, "k"); string(v2) != "abc" {
		t.Fatal("returned slice aliases the cache")
	}
	now = now.Add(time.Second)
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("expired entry returned")
	}
	if used, n, _ := m.Stats(); used != 0 || n != 0 {
		t.Fatalf("expired entry still accounted: %d bytes %d entries", used, n)
	}
}

type failing struct{ Cache }

func (failing) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("down")
}

func TestGetOrMissAndMetrics(t *testing.T) {
	reg := observability.NewRegistry()
	c := WithMetrics(failing{NewMemory(1<<20, 0)}, reg, "test")
	var seen error
	if v, ok := GetOrMiss(ctx, c, "k", func(err error) { seen = err }); ok || v != nil || seen == nil {
		t.Fatalf("error not degraded to a miss: %v %v %v", v, ok, seen)
	}
	if reg.Counter("cache_errors_total", `driver="test"`).Load() != 1 {
		t.Fatal("error not counted")
	}
	ok := WithMetrics(NewMemory(1<<20, 0), reg, "m")
	ok.Set(ctx, "a", []byte("1"), time.Minute)
	GetOrMiss(ctx, ok, "a", nil)
	GetOrMiss(ctx, ok, "b", nil)
	if reg.Counter("cache_hits_total", `driver="m"`).Load() != 1 || reg.Counter("cache_misses_total", `driver="m"`).Load() != 1 {
		t.Fatal("hit/miss not counted")
	}
}

func TestValkeyCache(t *testing.T) {
	cl := valkeytest.Client(t)
	v := &Valkey{Client: cl, Prefix: valkeytest.Prefix(t), MaxValueBytes: 1 << 10, Timeout: time.Second}
	if _, ok, err := v.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("miss: %v %v", ok, err)
	}
	if err := v.Set(ctx, "k", []byte("v\x00\r\n"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := v.Get(ctx, "k"); !ok || err != nil || string(got) != "v\x00\r\n" {
		t.Fatalf("%q %v %v", got, ok, err)
	}
	// Stored under the application prefix with the TTL.
	r, err := cl.Do(ctx, "PTTL", v.Prefix+"cache:k")
	if err != nil || r.Int <= 0 || r.Int > 60_000 {
		t.Fatalf("pttl %+v %v", r, err)
	}
	if err := v.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := v.Get(ctx, "k"); ok {
		t.Fatal("deleted key returned")
	}
	if err := v.Set(ctx, "big", make([]byte, 1025), time.Minute); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	if err := v.Set(ctx, "k", nil, -1); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("ttl: %v", err)
	}
	v.Set(ctx, "short", []byte("x"), 50*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if _, ok, _ := v.Get(ctx, "short"); ok {
		t.Fatal("expired key returned")
	}
}

// With Valkey unreachable every operation fails fast and GetOrMiss is a miss.
func TestValkeyCacheUnavailable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	cl := valkey.New(valkey.Options{Addr: addr})
	defer cl.Close()
	v := &Valkey{Client: cl, Prefix: "p:", Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, ok := GetOrMiss(ctx, v, "k", nil); ok {
		t.Fatal("hit from an unreachable server")
	}
	if err := v.Set(ctx, "k", []byte("x"), time.Minute); err == nil {
		t.Fatal("set succeeded")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}
