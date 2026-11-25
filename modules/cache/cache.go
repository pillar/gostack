// Package cache is a byte-value cache with TTLs: a bounded in-process LRU
// (driver memory) or Valkey (driver valkey, shared by all instances).
//
// Rule: a cache never holds business facts. Balances, ledger entries,
// inventory, idempotency records, sessions and anything else whose loss or
// staleness would change an outcome are read from the database. A cache may
// only hold data that is recomputable from the database and allowed to be
// briefly stale; every Get failure must be treated as a miss (see GetOrMiss)
// and every Set/Delete failure as "not cached". Nothing in the money/ledger
// paths uses this package.
package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/pillar/gostack/core/observability"
)

// Cache stores opaque values with a TTL.
type Cache interface {
	// Get returns the value and true, or false on a miss or expiry.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value for ttl (> 0). Values larger than the configured limit
	// are rejected with ErrValueTooLarge.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes key; deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

var (
	ErrValueTooLarge = errors.New("cache: value too large")
	ErrInvalidTTL    = errors.New("cache: ttl must be positive")
	ErrInvalidKey    = errors.New("cache: key must be 1..512 bytes")
)

// MaxKeyBytes bounds key length for both drivers.
const MaxKeyBytes = 512

func checkKey(key string) error {
	if len(key) == 0 || len(key) > MaxKeyBytes {
		return ErrInvalidKey
	}
	return nil
}

// GetOrMiss reads key and degrades any error to a miss: callers then load the
// value from the source of truth. onErr (optional) observes the error, e.g. to
// count it; it must not fail the request.
func GetOrMiss(ctx context.Context, c Cache, key string, onErr func(error)) ([]byte, bool) {
	v, ok, err := c.Get(ctx, key)
	if err != nil {
		if onErr != nil {
			onErr(err)
		}
		return nil, false
	}
	return v, ok
}

// Observed counts hits, misses and errors of the wrapped cache as
// cache_{hits,misses,errors}_total{driver="..."}.
type Observed struct {
	Cache
	hits, misses, errs *atomic.Int64
}

// WithMetrics wraps c with counters registered in reg.
func WithMetrics(c Cache, reg *observability.Registry, driver string) *Observed {
	l := `driver="` + driver + `"`
	return &Observed{Cache: c, hits: reg.Counter("cache_hits_total", l), misses: reg.Counter("cache_misses_total", l),
		errs: reg.Counter("cache_errors_total", l)}
}

func (o *Observed) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, ok, err := o.Cache.Get(ctx, key)
	switch {
	case err != nil:
		o.errs.Add(1)
	case ok:
		o.hits.Add(1)
	default:
		o.misses.Add(1)
	}
	return v, ok, err
}

func (o *Observed) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	err := o.Cache.Set(ctx, key, value, ttl)
	if err != nil {
		o.errs.Add(1)
	}
	return err
}

func (o *Observed) Delete(ctx context.Context, key string) error {
	err := o.Cache.Delete(ctx, key)
	if err != nil {
		o.errs.Add(1)
	}
	return err
}
