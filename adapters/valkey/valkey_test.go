package valkey_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/valkey"
	"github.com/pillar/gostack/adapters/valkeytest"
)

// fakeServer answers each command line-by-line with reply(cmd) ("" = never answer).
func fakeServer(t *testing.T, reply func(args []string) string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					args, err := readCommand(r)
					if err != nil {
						return
					}
					if out := reply(args); out != "" {
						c.Write([]byte(out))
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var n int
	if _, err := fmt.Sscanf(line, "*%d", &n); err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		if _, err := r.ReadString('\n'); err != nil {
			return nil, err
		}
		s, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		args[i] = strings.TrimSuffix(s, "\r\n")
	}
	return args, nil
}

func TestRepliesAndErrors(t *testing.T) {
	addr := fakeServer(t, func(a []string) string {
		switch a[0] {
		case "PING":
			return "+PONG\r\n"
		case "GET":
			if a[1] == "missing" {
				return "$-1\r\n"
			}
			return "$5\r\nhe\r\nl\r\n" // binary-safe bulk containing CRLF
		case "ARR":
			return "*3\r\n:1\r\n$-1\r\n*1\r\n+x\r\n"
		case "BAD":
			return "-ERR wrong\r\n"
		}
		return "?junk\r\n"
	})
	c := valkey.New(valkey.Options{Addr: addr, Timeout: time.Second, PoolSize: 2})
	defer c.Close()
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err := c.String(ctx, "GET", "k"); err != nil || s != "he\r\nl" {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := c.String(ctx, "GET", "missing"); !errors.Is(err, valkey.ErrNil) {
		t.Fatalf("missing: %v", err)
	}
	r, err := c.Do(ctx, "ARR")
	if err != nil || len(r.Array) != 3 || r.Array[0].Int != 1 || !r.Array[1].Nil || r.Array[2].Array[0].Str != "x" {
		t.Fatalf("%+v %v", r, err)
	}
	var se valkey.Error
	if _, err := c.Do(ctx, "BAD"); !errors.As(err, &se) || !strings.HasPrefix(string(se), "ERR") {
		t.Fatalf("server error: %v", err)
	}
	if _, err := c.Do(ctx, "JUNK"); err == nil {
		t.Fatal("protocol error not reported")
	}
	// The connection that saw junk was discarded; the client still works.
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// A server that never answers costs at most Timeout, and the stuck connection
// is not reused (a late reply can never be read by the next command).
func TestTimeoutDiscardsConnection(t *testing.T) {
	var mu sync.Mutex
	hang := true
	addr := fakeServer(t, func(a []string) string {
		mu.Lock()
		defer mu.Unlock()
		if hang {
			return ""
		}
		return "+PONG\r\n"
	})
	c := valkey.New(valkey.Options{Addr: addr, Timeout: 50 * time.Millisecond, PoolSize: 1})
	defer c.Close()
	start := time.Now()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected timeout")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("timeout took %v", d)
	}
	mu.Lock()
	hang = false
	mu.Unlock()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("after timeout: %v", err)
	}
	// A cancelled context aborts promptly even with a long Timeout.
	c2 := valkey.New(valkey.Options{Addr: addr, Timeout: 10 * time.Second})
	defer c2.Close()
	mu.Lock()
	hang = true
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start = time.Now()
	if err := c2.Ping(ctx); err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("ctx deadline not honoured: %v after %v", err, time.Since(start))
	}
}

func TestUnreachable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c := valkey.New(valkey.Options{Addr: addr, Timeout: 100 * time.Millisecond})
	defer c.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("ping to closed port succeeded")
	}
	c.Close()
	if err := c.Ping(context.Background()); !errors.Is(err, valkey.ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
}

func TestPoolBound(t *testing.T) {
	addr := fakeServer(t, func(a []string) string { time.Sleep(20 * time.Millisecond); return "+PONG\r\n" })
	c := valkey.New(valkey.Options{Addr: addr, Timeout: 2 * time.Second, PoolSize: 2})
	defer c.Close()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.Ping(context.Background()) }()
	}
	wg.Wait()
	// With PoolSize 2, a caller whose deadline expires while waiting gets an error.
	block := make(chan struct{})
	addr2 := fakeServer(t, func(a []string) string { <-block; return "+PONG\r\n" })
	c2 := valkey.New(valkey.Options{Addr: addr2, Timeout: 100 * time.Millisecond, PoolSize: 1})
	defer c2.Close()
	go c2.Ping(context.Background())
	time.Sleep(10 * time.Millisecond)
	if err := c2.Ping(context.Background()); err == nil {
		t.Fatal("expected pool wait timeout")
	}
	close(block)
}

func TestLiveServer(t *testing.T) {
	c := valkeytest.Client(t)
	p := valkeytest.Prefix(t)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x\x00\r\n", 100_000)
	if _, err := c.Do(ctx, "SET", p+"k", big, "PX", "60000"); err != nil {
		t.Fatal(err)
	}
	if s, err := c.String(ctx, "GET", p+"k"); err != nil || s != big {
		t.Fatalf("round trip of %d bytes: %d %v", len(big), len(s), err)
	}
	s := valkey.NewScript(`return {tonumber(ARGV[1]) + 1, KEYS[1]}`)
	if _, err := c.Do(ctx, "SCRIPT", "FLUSH"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(ctx, c, []string{"a"}, "41") // NOSCRIPT → EVAL fallback
	if err != nil || r.Array[0].Int != 42 || r.Array[1].Str != "a" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err = s.Run(ctx, c, []string{"b"}, "1"); err != nil || r.Array[0].Int != 2 {
		t.Fatalf("evalsha: %+v %v", r, err)
	}
}
