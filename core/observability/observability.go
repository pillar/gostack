// Package observability provides structured logging, a dependency-free metrics
// registry with Prometheus text exposition, and the HTTP access middleware.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pillar/gostack/core/httpx"
)

// NewLogger builds a slog logger. Attributes whose key looks sensitive are redacted.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: redact}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

var sensitive = []string{"secret", "token", "password", "authorization", "api_key", "cookie"}

func redact(_ []string, a slog.Attr) slog.Attr {
	k := strings.ToLower(a.Key)
	for _, s := range sensitive {
		if strings.Contains(k, s) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}

// Registry holds counters, gauges and histograms. Labels must be low-cardinality
// (route patterns and status classes, never user IDs).
type Registry struct {
	mu       sync.Mutex
	counters map[string]*atomic.Int64
	hists    map[string]*Histogram
	gauges   map[string]func() float64
}

func NewRegistry() *Registry {
	return &Registry{counters: map[string]*atomic.Int64{}, hists: map[string]*Histogram{}, gauges: map[string]func() float64{}}
}

// Counter returns the counter for name{labels} (labels already formatted, e.g. `route="x"`).
func (r *Registry) Counter(name, labels string) *atomic.Int64 {
	key := series(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[key]
	if !ok {
		c = &atomic.Int64{}
		r.counters[key] = c
	}
	return c
}

// Gauge registers a function evaluated at scrape time.
func (r *Registry) Gauge(name string, f func() float64) {
	r.mu.Lock()
	r.gauges[name] = f
	r.mu.Unlock()
}

func (r *Registry) Histogram(name, labels string) *Histogram {
	key := series(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hists[key]
	if !ok {
		h = &Histogram{name: name, labels: labels}
		r.hists[key] = h
	}
	return h
}

func series(name, labels string) string {
	if labels == "" {
		return name
	}
	return name + "{" + labels + "}"
}

// Histogram buckets in seconds.
var buckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1, 2.5, 5, 10}

type Histogram struct {
	name, labels string
	mu           sync.Mutex
	counts       [14]uint64
	sum          float64
	n            uint64
}

func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(buckets, v)
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.n++
	h.mu.Unlock()
}

// Render writes all series in Prometheus text format.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	keys := make([]string, 0, len(r.counters))
	for k := range r.counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s %d\n", k, r.counters[k].Load())
	}
	gk := make([]string, 0, len(r.gauges))
	for k := range r.gauges {
		gk = append(gk, k)
	}
	sort.Strings(gk)
	for _, k := range gk {
		fmt.Fprintf(w, "%s %g\n", k, r.gauges[k]())
	}
	hk := make([]string, 0, len(r.hists))
	for k := range r.hists {
		hk = append(hk, k)
	}
	sort.Strings(hk)
	hists := make([]*Histogram, 0, len(hk))
	for _, k := range hk {
		hists = append(hists, r.hists[k])
	}
	r.mu.Unlock()
	for _, h := range hists {
		h.mu.Lock()
		var cum uint64
		sep := ""
		if h.labels != "" {
			sep = ","
		}
		for i, b := range buckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "%s_bucket{%s%sle=\"%g\"} %d\n", h.name, h.labels, sep, b, cum)
		}
		cum += h.counts[len(buckets)]
		fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", h.name, h.labels, sep, cum)
		fmt.Fprintf(w, "%s %g\n%s %d\n", series(h.name+"_sum", h.labels), h.sum, series(h.name+"_count", h.labels), h.n)
		h.mu.Unlock()
	}
}

// RegisterRuntime adds process memory/goroutine gauges.
func (r *Registry) RegisterRuntime() {
	var mu sync.Mutex
	var ms runtime.MemStats
	var last time.Time
	read := func() *runtime.MemStats {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) > time.Second {
			runtime.ReadMemStats(&ms)
			last = time.Now()
		}
		return &ms
	}
	r.Gauge("go_goroutines", func() float64 { return float64(runtime.NumGoroutine()) })
	r.Gauge("go_heap_alloc_bytes", func() float64 { return float64(read().HeapAlloc) })
	r.Gauge("go_sys_bytes", func() float64 { return float64(read().Sys) })
	r.Gauge("go_gc_cycles_total", func() float64 { return float64(read().NumGC) })
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.Render(w)
	})
}

// AccessLog records latency/status metrics per route pattern and logs each request.
func AccessLog(reg *Registry, log *slog.Logger) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &httpx.StatusRecorder{ResponseWriter: w}
			route := new(string)
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), routeKey{}, route)))
			if rec.Status == 0 {
				rec.Status = http.StatusOK
			}
			d := time.Since(start)
			if *route == "" {
				*route = "unmatched"
			}
			labels := fmt.Sprintf("route=%q", *route)
			reg.Counter("http_requests_total", fmt.Sprintf("%s,class=\"%dxx\"", labels, rec.Status/100)).Add(1)
			reg.Histogram("http_request_duration_seconds", labels).Observe(d.Seconds())
			lvl := slog.LevelInfo
			if rec.Status >= 500 {
				lvl = slog.LevelError
			}
			log.LogAttrs(r.Context(), lvl, "http",
				slog.String("request_id", httpx.RequestID(r.Context())),
				slog.String("method", r.Method), slog.String("route", *route),
				slog.Int("status", rec.Status), slog.Float64("ms", math.Round(d.Seconds()*1e5)/100))
		})
	}
}

type routeKey struct{}

// CaptureRoute wraps the ServeMux (innermost) and reports the matched pattern to
// AccessLog; the mux sets Pattern only on the request copy it receives.
func CaptureRoute(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if p, ok := r.Context().Value(routeKey{}).(*string); ok {
			*p = r.Pattern
		}
	})
}

// Stderr is the default log destination (systemd journal captures it).
var Stderr io.Writer = os.Stderr
