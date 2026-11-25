package cache

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/pillar/gostack/adapters/valkey"
)

// Valkey is a cache shared by all instances. Keys are stored as
// <prefix>cache:<key>; every value has a TTL (PX), so Valkey's own eviction
// policy and expiry bound memory. Each operation is capped by Timeout.
type Valkey struct {
	Client        *valkey.Client
	Prefix        string        // per application, e.g. "mygame:"
	MaxValueBytes int64         // default 1 MiB
	Timeout       time.Duration // default 100ms
}

func (v *Valkey) key(k string) string { return v.Prefix + "cache:" + k }

func (v *Valkey) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	t := v.Timeout
	if t <= 0 {
		t = 100 * time.Millisecond
	}
	return context.WithTimeout(ctx, t)
}

func (v *Valkey) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := checkKey(key); err != nil {
		return nil, false, err
	}
	ctx, cancel := v.ctx(ctx)
	defer cancel()
	s, err := v.Client.String(ctx, "GET", v.key(key))
	if errors.Is(err, valkey.ErrNil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(s), true, nil
}

func (v *Valkey) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	limit := v.MaxValueBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	if int64(len(value)) > limit {
		return ErrValueTooLarge
	}
	ms := max(ttl.Milliseconds(), 1)
	ctx, cancel := v.ctx(ctx)
	defer cancel()
	_, err := v.Client.Do(ctx, "SET", v.key(key), string(value), "PX", strconv.FormatInt(ms, 10))
	return err
}

func (v *Valkey) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	ctx, cancel := v.ctx(ctx)
	defer cancel()
	_, err := v.Client.Do(ctx, "DEL", v.key(key))
	return err
}
