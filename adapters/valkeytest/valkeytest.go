// Package valkeytest runs Valkey-backed tests when GOSTACK_TEST_VALKEY_ADDR
// points at a disposable Valkey server (host:port, no password), e.g. a local
//
//	valkey-server --port 56379 --save '' --appendonly no
//
// Every test gets its own random key prefix; keys under it are deleted in
// t.Cleanup. Without the variable the helpers skip or report "not enabled".
package valkeytest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/valkey"
	"github.com/pillar/gostack/core/config"
)

// EnvAddr is the variable that enables Valkey test runs.
const EnvAddr = "GOSTACK_TEST_VALKEY_ADDR"

// Addr returns the server address ("" when disabled).
func Addr() string { return os.Getenv(EnvAddr) }

// Enabled reports whether Valkey tests run.
func Enabled() bool { return Addr() != "" }

// Require skips t unless Valkey tests are enabled and returns the address.
func Require(t testing.TB) string {
	t.Helper()
	if !Enabled() {
		t.Skip(EnvAddr + " not set")
	}
	return Addr()
}

// Prefix returns a fresh key prefix whose keys are deleted when t ends.
func Prefix(t testing.TB) string {
	t.Helper()
	addr := Require(t)
	var b [6]byte
	_, _ = rand.Read(b[:])
	p := "gstest" + hex.EncodeToString(b[:]) + ":"
	t.Cleanup(func() {
		c := valkey.New(valkey.Options{Addr: addr, Timeout: 5 * time.Second})
		defer c.Close()
		ctx := context.Background()
		cursor := "0"
		for {
			r, err := c.Do(ctx, "SCAN", cursor, "MATCH", p+"*", "COUNT", "1000")
			if err != nil || len(r.Array) != 2 {
				return // best effort: every key has a TTL anyway
			}
			for _, k := range r.Array[1].Array {
				_, _ = c.Do(ctx, "DEL", k.Str)
			}
			if cursor = r.Array[0].Str; cursor == "0" {
				return
			}
		}
	})
	return p
}

// Client returns a client for the test server, closed when t ends.
func Client(t testing.TB) *valkey.Client {
	t.Helper()
	c := valkey.New(valkey.Options{Addr: Require(t), Timeout: 2 * time.Second})
	t.Cleanup(func() { c.Close() })
	return c
}

// UseValkey switches a profile scale cfg to the global rate limiter and the
// Valkey cache with a fresh key prefix when Valkey tests are enabled, and
// reports whether it did.
func UseValkey(t testing.TB, cfg *config.Config) bool {
	t.Helper()
	if !Enabled() || cfg.Profile != "scale" {
		return false
	}
	cfg.RateLimit.Driver, cfg.RateLimit.AcceptPerInstance = "valkey", false
	cfg.Cache.Driver = "valkey"
	cfg.Valkey.Address = Addr()
	cfg.Valkey.KeyPrefix = Prefix(t)
	// Generous under -race on a loaded machine; production keeps the 50ms default.
	cfg.RateLimit.Timeout = time.Second
	return true
}
