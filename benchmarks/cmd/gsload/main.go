// Command gsload is an open-loop (constant arrival rate) HTTP load generator for
// the GoStack PRD §13/§23 scenarios. Requests are scheduled at fixed intervals
// regardless of response time; latency is measured from the scheduled send time
// (coordinated-omission corrected) and, separately, from the actual send time.
//
//	gsload -url http://127.0.0.1:18080 -scenario pointread -rate 500 -warmup 30s -duration 90s -pid $SERVER_PID -out r.json
//	gsload ... -scenario board -sat                # step the rate up until the budget breaks
//	gsload ... -scenario ws -conns 3000 -duration 120s [-ws-msg-interval 30s]
//	gsload ... -scenario idlehttp -conns 3000 -duration 90s
//	gsload -list
//
// Results are JSON; every result carries label "local, not acceptance" unless
// -label is given (use it only for runs on the PRD 1C1G target with a separate load host).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pillar/gostack/benchmarks/internal/hist"
)

type opStats struct {
	Count   int64            `json:"count"`
	OK      int64            `json:"ok"`
	Expect  int64            `json:"expected_rejections"`
	Errors  int64            `json:"errors"`
	Latency hist.Summary     `json:"latency"`
	Kinds   map[string]int64 `json:"kinds,omitempty"`
	h       *hist.H
}

// ServerSample is resource usage of the server process during the steady phase.
type ServerSample struct {
	PID         int     `json:"pid"`
	RSSStartKiB int64   `json:"rss_start_kib"`
	RSSMaxKiB   int64   `json:"rss_max_kib"`
	RSSEndKiB   int64   `json:"rss_end_kib"`
	CPUSeconds  float64 `json:"cpu_seconds"`
	CPUUtil     float64 `json:"cpu_util"` // CPU seconds / wall seconds (1.0 = one core)
}

// Result is one run at one offered rate.
type Result struct {
	Label        string              `json:"label"`
	Scenario     string              `json:"scenario"`
	Description  string              `json:"description"`
	Budget       Budget              `json:"budget"`
	OfferedRPS   float64             `json:"offered_rps"`
	WarmupS      float64             `json:"warmup_s"`
	SteadyS      float64             `json:"steady_s"`
	StartedAt    time.Time           `json:"started_at"`
	Scheduled    int64               `json:"scheduled"`
	Completed    int64               `json:"completed"`
	AchievedRPS  float64             `json:"achieved_rps"`
	OK           int64               `json:"ok"`
	Expected     int64               `json:"expected_rejections"`
	Errors       int64               `json:"errors"`
	ErrorRate    float64             `json:"error_rate"` // errors / (scheduled - expected)
	ErrorKinds   map[string]int64    `json:"error_kinds"`
	ErrorSamples map[string]string   `json:"error_samples,omitempty"`
	Latency      hist.Summary        `json:"latency_corrected"`
	Service      hist.Summary        `json:"latency_service"`
	SchedLag     hist.Summary        `json:"client_schedule_lag"` // dispatch time - scheduled time (client-side error)
	Ops          map[string]*opStats `json:"ops"`
	BudgetMs     float64             `json:"budget_measured_ms"`
	Pass         bool                `json:"pass_local"`
	FailReasons  []string            `json:"fail_reasons,omitempty"`
	Server       *ServerSample       `json:"server,omitempty"`
	ClientDrops  int64               `json:"client_dropped"`
	MaxInflight  int64               `json:"client_max_inflight"`
	ReplaysOK    int64               `json:"claim_replays_ok,omitempty"`
	ReplayBad    int64               `json:"claim_replays_mismatch,omitempty"`
	ClientGo     string              `json:"client_go"`
	ClientProcs  int                 `json:"client_gomaxprocs"`
	Extra        map[string]any      `json:"extra,omitempty"`
}

type flags struct {
	url, scenario, out, statePath, label string
	rate, step, maxRate                  float64
	warmup, duration, satWarmup, satDur  time.Duration
	pid, players, conns, maxInflight     int
	seed                                 uint64
	sat                                  bool
	connRate                             float64
	wsInterval                           time.Duration
}

func main() {
	var f flags
	flag.StringVar(&f.url, "url", "http://127.0.0.1:18080", "server base URL")
	flag.StringVar(&f.scenario, "scenario", "", "scenario name (see -list), ws or idlehttp")
	flag.Float64Var(&f.rate, "rate", 0, "offered requests/s (default: PRD target)")
	flag.DurationVar(&f.warmup, "warmup", 30*time.Second, "warmup (not recorded)")
	flag.DurationVar(&f.duration, "duration", 90*time.Second, "steady phase (recorded)")
	flag.BoolVar(&f.sat, "sat", false, "saturation search: step the rate from -rate (default target) by -step until the budget fails")
	flag.Float64Var(&f.step, "step", 1.5, "saturation rate multiplier")
	flag.Float64Var(&f.maxRate, "max-rate", 20000, "saturation upper bound")
	flag.DurationVar(&f.satWarmup, "sat-warmup", 5*time.Second, "warmup per saturation step")
	flag.DurationVar(&f.satDur, "sat-duration", 20*time.Second, "steady per saturation step")
	flag.IntVar(&f.pid, "pid", 0, "server PID to sample RSS/CPU with ps")
	flag.IntVar(&f.players, "players", 100_000, "seeded players")
	flag.Uint64Var(&f.seed, "seed", 20261125, "seed used by gsseed")
	flag.IntVar(&f.conns, "conns", 3000, "connections (ws, idlehttp)")
	flag.Float64Var(&f.connRate, "conn-rate", 500, "connections opened per second (ws, idlehttp)")
	flag.DurationVar(&f.wsInterval, "ws-msg-interval", 0, "ws: each connection sends a ping message at this interval (0 = idle)")
	flag.IntVar(&f.maxInflight, "max-inflight", 4096, "client in-flight cap; arrivals beyond it are counted as client_dropped errors")
	flag.StringVar(&f.out, "out", "", "write JSON here (default stdout)")
	flag.StringVar(&f.statePath, "state", "gsload-state.json", "save revision/cursor state carried between runs")
	flag.StringVar(&f.label, "label", "local, not acceptance", "label stored in results")
	list := flag.Bool("list", false, "list scenarios")
	flag.Parse()
	if *list {
		for _, s := range scenarios {
			fmt.Printf("%-14s %s target %.0f rps p%.0f<=%.0fms  %s\n", s.name, s.budget.PRD, s.budget.TargetRPS, s.budget.Percentile*100, s.budget.LimitMs, s.desc)
		}
		fmt.Println("ws             §13 空闲/活跃长连接 (WebSocket, realtime module)")
		fmt.Println("idlehttp       idle HTTP/1.1 keep-alive connections")
		return
	}
	var out any
	switch f.scenario {
	case "ws":
		out = runWS(f)
	case "idlehttp":
		out = runIdleHTTP(f)
	default:
		sc, ok := findScenario(f.scenario)
		if !ok {
			log.Fatalf("unknown scenario %q (-list)", f.scenario)
		}
		c := newClient(f)
		defer saveState(f.statePath, c.state)
		rate := f.rate
		if rate == 0 {
			rate = sc.budget.TargetRPS
		}
		if !f.sat {
			out = runRate(f, c, sc, rate, f.warmup, f.duration)
			break
		}
		var steps []*Result
		sat := 0.0
		for r := rate; r <= f.maxRate; r *= f.step {
			res := runRate(f, c, sc, r, f.satWarmup, f.satDur)
			steps = append(steps, res)
			log.Printf("sat %s %.0f rps: achieved %.0f p%.0f=%.1fms err=%.4f pass=%v %v", sc.name, r, res.AchievedRPS, sc.budget.Percentile*100, res.BudgetMs, res.ErrorRate, res.Pass, res.FailReasons)
			if !res.Pass {
				break
			}
			sat = r
			saveState(f.statePath, c.state)
		}
		out = map[string]any{"label": f.label, "scenario": sc.name, "budget": sc.budget, "mode": "saturation",
			"step_warmup_s": f.satWarmup.Seconds(), "step_steady_s": f.satDur.Seconds(), "step_factor": f.step,
			"highest_passing_rps": sat, "steps": steps}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if f.out == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(f.out, append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        0,
		MaxIdleConnsPerHost: 4096,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
}

func newClient(f flags) *client {
	c := &client{base: strings.TrimRight(f.url, "/"), seed: f.seed, players: f.players,
		runID: strconv.FormatInt(time.Now().UnixNano(), 36),
		hc:    &http.Client{Transport: newTransport(), Timeout: 15 * time.Second}}
	c.state = &revState{Revs: map[string]int64{}}
	if b, err := os.ReadFile(f.statePath); err == nil {
		if err := json.Unmarshal(b, c.state); err != nil {
			log.Fatalf("state %s: %v", f.statePath, err)
		}
	}
	c.saveCur.Store(c.state.Save)
	c.econCur.Store(c.state.Econ)
	c.state.Save, c.state.Econ = 0, 0
	stateOwner = c
	return c
}

var stateOwner *client

func saveState(path string, s *revState) {
	if stateOwner != nil {
		s.Save, s.Econ = stateOwner.saveCur.Load(), stateOwner.econCur.Load()
	}
	s.mu.Lock()
	b, err := json.Marshal(s)
	s.mu.Unlock()
	if err == nil {
		err = os.WriteFile(path, b, 0o644)
	}
	if err != nil {
		log.Printf("save state: %v", err)
	}
}

// --- server sampling -------------------------------------------------------

func psSample(pid int) (rssKiB int64, cpu float64, err error) {
	out, err := exec.Command("ps", "-o", "rss=,time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, err
	}
	fs := strings.Fields(string(out))
	if len(fs) < 2 {
		return 0, 0, fmt.Errorf("ps: %q", out)
	}
	rssKiB, _ = strconv.ParseInt(fs[0], 10, 64)
	// time is [[dd-]hh:]mm:ss[.cc]
	t := fs[1]
	if d, rest, ok := strings.Cut(t, "-"); ok {
		days, _ := strconv.ParseFloat(d, 64)
		cpu += days * 86400
		t = rest
	}
	parts := strings.Split(t, ":")
	mul := 1.0
	for i := len(parts) - 1; i >= 0; i-- {
		v, _ := strconv.ParseFloat(parts[i], 64)
		cpu += v * mul
		mul *= 60
	}
	return rssKiB, cpu, nil
}

type sampler struct {
	pid    int
	s      ServerSample
	cpu0   float64
	t0     time.Time
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	failed bool
}

func startSampler(pid int) *sampler {
	if pid == 0 {
		return nil
	}
	sp := &sampler{pid: pid, stop: make(chan struct{}), done: make(chan struct{}), t0: time.Now()}
	sp.s.PID = pid
	rss, cpu, err := psSample(pid)
	if err != nil {
		log.Printf("sampler: %v", err)
		return nil
	}
	sp.s.RSSStartKiB, sp.s.RSSMaxKiB, sp.cpu0 = rss, rss, cpu
	go func() {
		defer close(sp.done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-sp.stop:
				return
			case <-t.C:
				if rss, _, err := psSample(pid); err == nil {
					sp.mu.Lock()
					sp.s.RSSMaxKiB = max(sp.s.RSSMaxKiB, rss)
					sp.mu.Unlock()
				}
			}
		}
	}()
	return sp
}

func (sp *sampler) finish() *ServerSample {
	if sp == nil {
		return nil
	}
	close(sp.stop)
	<-sp.done
	rss, cpu, err := psSample(sp.pid)
	if err == nil {
		sp.s.RSSEndKiB = rss
		sp.s.RSSMaxKiB = max(sp.s.RSSMaxKiB, rss)
		sp.s.CPUSeconds = cpu - sp.cpu0
		sp.s.CPUUtil = sp.s.CPUSeconds / time.Since(sp.t0).Seconds()
	}
	return &sp.s
}

// --- open-loop runner ------------------------------------------------------

func runRate(f flags, c *client, sc scenario, rate float64, warmup, steady time.Duration) *Result {
	res := &Result{Label: f.label, Scenario: sc.name, Description: sc.desc, Budget: sc.budget, OfferedRPS: rate,
		WarmupS: warmup.Seconds(), SteadyS: steady.Seconds(), ErrorKinds: map[string]int64{}, ErrorSamples: map[string]string{}, Ops: map[string]*opStats{},
		ClientGo: runtime.Version(), ClientProcs: runtime.GOMAXPROCS(0)}
	total := warmup + steady
	var weights []int
	sum := 0
	for _, w := range sc.mix {
		sum += w.weight
		weights = append(weights, sum)
	}
	pick := func(r *rand.Rand) string {
		x := r.IntN(sum)
		i := sort.SearchInts(weights, x+1)
		return sc.mix[i].op
	}
	corrected, service, lagH := hist.New(), hist.New(), hist.New()
	var mu sync.Mutex
	var inflight, maxInflight, drops, scheduled, completed atomic.Int64
	var wg sync.WaitGroup
	var sp *sampler
	ctx := context.Background()
	start := time.Now().Add(20 * time.Millisecond)
	steadyAt := start.Add(warmup)
	res.StartedAt = steadyAt
	samplerStarted := false
	for i := int64(0); ; i++ {
		at := start.Add(time.Duration(float64(i) * float64(time.Second) / rate))
		if at.Sub(start) >= total {
			break
		}
		// Sleep coarsely, then spin: OS timer slack (≈1-2 ms on macOS) would
		// otherwise be charged to the server as latency.
		if d := time.Until(at); d > 2*time.Millisecond {
			time.Sleep(d - 2*time.Millisecond)
		}
		for time.Now().Before(at) {
			runtime.Gosched()
		}
		rec := !at.Before(steadyAt)
		if rec {
			lagH.Record(time.Since(at))
		}
		if rec && !samplerStarted {
			samplerStarted = true
			sp = startSampler(f.pid)
		}
		if rec {
			scheduled.Add(1)
		}
		if n := inflight.Add(1); n > int64(f.maxInflight) {
			inflight.Add(-1)
			if rec {
				drops.Add(1)
			}
			continue
		} else if n > maxInflight.Load() {
			maxInflight.Store(n)
		}
		wg.Add(1)
		go func(i int64, at time.Time, rec bool) {
			defer wg.Done()
			defer inflight.Add(-1)
			r := rand.New(rand.NewPCG(f.seed, uint64(i)^uint64(rate*1000)<<40))
			op := pick(r)
			sent := time.Now()
			o := c.run(ctx, op, r)
			done := time.Now()
			if !rec {
				return
			}
			completed.Add(1)
			corrected.Record(done.Sub(at))
			service.Record(done.Sub(sent))
			mu.Lock()
			s := res.Ops[o.op]
			if s == nil {
				s = &opStats{h: hist.New(), Kinds: map[string]int64{}}
				res.Ops[o.op] = s
			}
			s.Count++
			s.h.Record(done.Sub(at))
			switch {
			case o.kind == "":
				s.OK++
				res.OK++
			case strings.HasPrefix(o.kind, "expected:"):
				s.Expect++
				res.Expected++
				s.Kinds[o.kind]++
			default:
				s.Errors++
				res.Errors++
				s.Kinds[o.kind]++
				res.ErrorKinds[o.kind]++
				if _, ok := res.ErrorSamples[o.kind]; !ok && o.errMsg != "" {
					res.ErrorSamples[o.kind] = o.errMsg
				}
			}
			mu.Unlock()
		}(i, at, rec)
	}
	waitCh := make(chan struct{})
	go func() { wg.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(20 * time.Second):
		log.Printf("warning: %d requests still in flight after 20s", inflight.Load())
	}
	res.Server = sp.finish()
	mu.Lock()
	defer mu.Unlock()
	res.Scheduled, res.Completed, res.ClientDrops, res.MaxInflight = scheduled.Load(), completed.Load(), drops.Load(), maxInflight.Load()
	res.Errors += res.ClientDrops + (res.Scheduled - res.Completed - res.ClientDrops) // drops and never-completed count as errors
	if res.ClientDrops > 0 {
		res.ErrorKinds["client_dropped"] = res.ClientDrops
	}
	if lost := res.Scheduled - res.Completed - res.ClientDrops; lost > 0 {
		res.ErrorKinds["incomplete"] = lost
	}
	res.AchievedRPS = float64(res.Completed) / steady.Seconds()
	if den := res.Scheduled - res.Expected; den > 0 {
		res.ErrorRate = float64(res.Errors) / float64(den)
	}
	res.Latency, res.Service, res.SchedLag = corrected.Summary(), service.Summary(), lagH.Summary()
	for _, s := range res.Ops {
		s.Latency = s.h.Summary()
		if len(s.Kinds) == 0 {
			s.Kinds = nil
		}
	}
	res.ReplaysOK, res.ReplayBad = c.replays.ok.Swap(0), c.replays.mismatch.Swap(0)
	res.BudgetMs = corrected.Quantile(sc.budget.Percentile)
	res.Pass = true
	if res.BudgetMs > sc.budget.LimitMs {
		res.Pass = false
		res.FailReasons = append(res.FailReasons, fmt.Sprintf("p%g %.1fms > %.0fms", sc.budget.Percentile*100, res.BudgetMs, sc.budget.LimitMs))
	}
	if res.ErrorRate > sc.budget.MaxErrRate {
		res.Pass = false
		res.FailReasons = append(res.FailReasons, fmt.Sprintf("error rate %.4f > %.4f", res.ErrorRate, sc.budget.MaxErrRate))
	}
	if res.AchievedRPS < 0.95*rate {
		res.Pass = false
		res.FailReasons = append(res.FailReasons, fmt.Sprintf("achieved %.0f < 95%% of offered %.0f", res.AchievedRPS, rate))
	}
	if res.ReplayBad > 0 {
		res.Pass = false
		res.FailReasons = append(res.FailReasons, fmt.Sprintf("%d idempotent replays returned a different claim", res.ReplayBad))
	}
	log.Printf("%s %.0f rps: ok=%d expected=%d errors=%d p50=%.1f p95=%.1f p99=%.1f max=%.1f ms pass=%v",
		sc.name, rate, res.OK, res.Expected, res.Errors, res.Latency.P50, res.Latency.P95, res.Latency.P99, res.Latency.Max, res.Pass)
	return res
}
