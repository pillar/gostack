// Package hist is a small log-linear latency histogram (HDR-style): values are
// recorded in microseconds into buckets whose width is at most 1/128 of the
// value, so every reported quantile is within 0.8% of the true sample value.
package hist

import (
	"math/bits"
	"sync"
	"time"
)

const subBits = 7 // 128 linear sub-buckets per power of two
const sub = 1 << subBits

// H is safe for concurrent use.
type H struct {
	mu     sync.Mutex
	counts []uint64
	n      uint64
	sum    float64
	min    uint64
	max    uint64
}

func New() *H { return &H{counts: make([]uint64, 64*sub)} }

func index(v uint64) int {
	if v < sub {
		return int(v)
	}
	exp := bits.Len64(v) - subBits - 1 // v>>exp in [128,256)
	return (exp+1)*sub + int(v>>uint(exp)) - sub
}

// upper returns the largest value that maps to bucket i (conservative report).
func upper(i int) uint64 {
	if i < sub {
		return uint64(i)
	}
	exp := i/sub - 1
	m := uint64(i%sub + sub)
	return ((m + 1) << uint(exp)) - 1
}

// Record adds one duration.
func (h *H) Record(d time.Duration) {
	v := uint64(max(d.Microseconds(), 0))
	h.mu.Lock()
	h.counts[index(v)]++
	if h.n == 0 || v < h.min {
		h.min = v
	}
	if v > h.max {
		h.max = v
	}
	h.n++
	h.sum += float64(v)
	h.mu.Unlock()
}

// Merge adds o's samples into h.
func (h *H) Merge(o *H) {
	o.mu.Lock()
	defer o.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range o.counts {
		h.counts[i] += c
	}
	if o.n > 0 && (h.n == 0 || o.min < h.min) {
		h.min = o.min
	}
	h.max = max(h.max, o.max)
	h.n += o.n
	h.sum += o.sum
}

// Count returns the number of samples.
func (h *H) Count() uint64 { h.mu.Lock(); defer h.mu.Unlock(); return h.n }

// Quantile returns the q-quantile (0..1) in milliseconds (bucket upper bound,
// capped at the observed maximum).
func (h *H) Quantile(q float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n == 0 {
		return 0
	}
	rank := uint64(q*float64(h.n) + 0.5)
	rank = min(max(rank, 1), h.n)
	var seen uint64
	for i, c := range h.counts {
		seen += c
		if seen >= rank {
			return float64(min(upper(i), h.max)) / 1000
		}
	}
	return float64(h.max) / 1000
}

// Summary is the JSON form of a histogram (milliseconds).
type Summary struct {
	Count uint64  `json:"count"`
	Min   float64 `json:"min_ms"`
	Mean  float64 `json:"mean_ms"`
	P50   float64 `json:"p50_ms"`
	P90   float64 `json:"p90_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	P999  float64 `json:"p999_ms"`
	Max   float64 `json:"max_ms"`
}

func (h *H) Summary() Summary {
	s := Summary{Count: h.Count(), P50: h.Quantile(.5), P90: h.Quantile(.9), P95: h.Quantile(.95), P99: h.Quantile(.99), P999: h.Quantile(.999)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n > 0 {
		s.Min, s.Max, s.Mean = float64(h.min)/1000, float64(h.max)/1000, h.sum/float64(h.n)/1000
	}
	return s
}
