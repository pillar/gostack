package hist

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

func TestIndexUpperMonotonic(t *testing.T) {
	prev := -1
	for v := uint64(0); v < 1<<22; v += 1 + v/1000 {
		i := index(v)
		if i < prev {
			t.Fatalf("index not monotonic at %d", v)
		}
		if upper(i) < v {
			t.Fatalf("upper(%d)=%d < %d", i, upper(i), v)
		}
		if i > 0 && upper(i-1) >= v {
			t.Fatalf("value %d also fits bucket %d", v, i-1)
		}
		prev = i
	}
}

func TestQuantileAccuracy(t *testing.T) {
	h := New()
	r := rand.New(rand.NewPCG(1, 2))
	vals := make([]float64, 100000)
	for i := range vals {
		us := uint64(r.ExpFloat64()*5000) + 50
		vals[i] = float64(us) / 1000
		h.Record(time.Duration(us) * time.Microsecond)
	}
	slices.Sort(vals)
	for _, q := range []float64{.5, .9, .99, .999} {
		want := vals[int(q*float64(len(vals)))-1]
		got := h.Quantile(q)
		if math.Abs(got-want)/want > 0.01 {
			t.Errorf("q%.3f got %.3f want %.3f", q, got, want)
		}
	}
}
