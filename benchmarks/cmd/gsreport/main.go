// Command gsreport renders the Markdown tables of docs/BENCHMARK_REPORT.md from
// a results directory written by scripts/run_local.sh.
//
//	gsreport benchmarks/results/2026-11-25
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

type summary struct {
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	P999 float64 `json:"p999_ms"`
	Max  float64 `json:"max_ms"`
}

type budget struct {
	PRD        string  `json:"prd"`
	TargetRPS  float64 `json:"target_rps"`
	Percentile float64 `json:"percentile"`
	LimitMs    float64 `json:"limit_ms"`
}

type server struct {
	RSSMax  int64   `json:"rss_max_kib"`
	RSSEnd  int64   `json:"rss_end_kib"`
	CPUUtil float64 `json:"cpu_util"`
}

type result struct {
	Scenario    string            `json:"scenario"`
	Budget      budget            `json:"budget"`
	Offered     float64           `json:"offered_rps"`
	Achieved    float64           `json:"achieved_rps"`
	ErrorRate   float64           `json:"error_rate"`
	Errors      int64             `json:"errors"`
	Expected    int64             `json:"expected_rejections"`
	ErrorKinds  map[string]int64  `json:"error_kinds"`
	Samples     map[string]string `json:"error_samples"`
	Latency     summary           `json:"latency_corrected"`
	Service     summary           `json:"latency_service"`
	Lag         summary           `json:"client_schedule_lag"`
	BudgetMs    float64           `json:"budget_measured_ms"`
	Pass        bool              `json:"pass_local"`
	FailReasons []string          `json:"fail_reasons"`
	Server      *server           `json:"server"`
	ReplaysOK   int64             `json:"claim_replays_ok"`
	ReplayBad   int64             `json:"claim_replays_mismatch"`
}

type sat struct {
	Scenario string    `json:"scenario"`
	Budget   budget    `json:"budget"`
	Highest  float64   `json:"highest_passing_rps"`
	Steps    []*result `json:"steps"`
}

type conn struct {
	Scenario   string           `json:"scenario"`
	Desc       string           `json:"description"`
	Target     int              `json:"target_connections"`
	Opened     int64            `json:"opened"`
	OpenErrors map[string]int64 `json:"open_errors"`
	Hold       float64          `json:"hold_s"`
	Alive      int64            `json:"alive_at_end"`
	Connect    summary          `json:"connect_latency"`
	MsgSent    int64            `json:"messages_sent"`
	MsgRTT     *summary         `json:"message_rtt"`
	MsgErrors  int64            `json:"message_errors"`
	Server     *server          `json:"server"`
}

func load(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		log.Fatalf("%s: %v", path, err)
	}
	return true
}

func mib(kib int64) string { return fmt.Sprintf("%.0f", float64(kib)/1024) }

func kinds(m map[string]int64) string {
	var s []string
	for k, v := range m {
		s = append(s, fmt.Sprintf("%s×%d", k, v))
	}
	return strings.Join(s, ", ")
}

var order = []string{"memory", "config_player", "pointread", "mixed9010", "fullwrite", "board", "save8k", "economy", "gamemix"}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: gsreport <results dir>")
	}
	dir := os.Args[1]
	fmt.Println("### 目标速率（预热 + 稳态，开环）")
	fmt.Println()
	fmt.Println("| 场景 | PRD 目标 | 施加/完成 RPS | 预算分位实测 | P50 | P99 | P99.9 | max | 服务延迟 P99 | 错误率 | 服务端 RSS 峰值 MiB | 服务端 CPU | 本机判定 |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|---|---|")
	for _, s := range order {
		var r result
		if !load(filepath.Join(dir, "target_"+s+".json"), &r) {
			continue
		}
		srv := "-"
		cpu := "-"
		if r.Server != nil {
			srv, cpu = mib(r.Server.RSSMax), fmt.Sprintf("%.0f%%", r.Server.CPUUtil*100)
		}
		verdict := "通过"
		if !r.Pass {
			verdict = "未通过: " + strings.Join(r.FailReasons, "; ")
		}
		fmt.Printf("| %s | %s %.0f RPS P%.0f≤%.0fms | %.0f / %.1f | %.2f ms | %.2f | %.2f | %.2f | %.1f | %.2f | %.4f%% | %s | %s | %s |\n",
			s, r.Budget.PRD, r.Budget.TargetRPS, r.Budget.Percentile*100, r.Budget.LimitMs, r.Offered, r.Achieved, r.BudgetMs,
			r.Latency.P50, r.Latency.P99, r.Latency.P999, r.Latency.Max, r.Service.P99, r.ErrorRate*100, srv, cpu, verdict)
	}
	fmt.Println()
	fmt.Println("### 饱和搜索（每步 ×1.5，直到预算分位、错误率或吞吐失败）")
	fmt.Println()
	fmt.Println("| 场景 | 最高通过速率 RPS | 首个失败步 RPS | 失败步实测 | 失败原因 | 失败步错误类型 | 失败步服务端 RSS 峰值 MiB / CPU |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, s := range order {
		var r sat
		if !load(filepath.Join(dir, "sat_"+s+".json"), &r) {
			continue
		}
		fail := "-"
		got, why, ek, srv := "-", "-", "-", "-"
		if n := len(r.Steps); n > 0 && !r.Steps[n-1].Pass {
			f := r.Steps[n-1]
			fail = fmt.Sprintf("%.0f", f.Offered)
			got = fmt.Sprintf("完成 %.0f RPS, P%.0f %.1f ms", f.Achieved, r.Budget.Percentile*100, f.BudgetMs)
			why = strings.Join(f.FailReasons, "; ")
			ek = kinds(f.ErrorKinds)
			for k, v := range f.Samples {
				ek += fmt.Sprintf(" (%s: %q)", k, v)
			}
			if f.Server != nil {
				srv = fmt.Sprintf("%s / %.0f%%", mib(f.Server.RSSMax), f.Server.CPUUtil*100)
			}
		}
		fmt.Printf("| %s | %.0f | %s | %s | %s | %s | %s |\n", s, r.Highest, fail, got, why, ek, srv)
	}
	fmt.Println()
	fmt.Println("### 长连接")
	fmt.Println()
	fmt.Println("| 运行 | 目标连接 | 建立 | 保持 s | 结束时存活 | 建连 P99 ms | 消息数 / RTT P99 ms / 错误 | 服务端 RSS 峰值 MiB | 服务端 CPU |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|")
	for _, n := range []string{"conns_ws_idle", "conns_ws_active", "conns_http_idle"} {
		var c conn
		if !load(filepath.Join(dir, n+".json"), &c) {
			continue
		}
		msg := "-"
		if c.MsgRTT != nil {
			msg = fmt.Sprintf("%d / %.2f / %d", c.MsgSent, c.MsgRTT.P99, c.MsgErrors)
		}
		srv, cpu := "-", "-"
		if c.Server != nil {
			srv, cpu = mib(c.Server.RSSMax), fmt.Sprintf("%.1f%%", c.Server.CPUUtil*100)
		}
		fmt.Printf("| %s | %d | %d | %.0f | %d | %.2f | %s | %s | %s |\n", n, c.Target, c.Opened, c.Hold, c.Alive, c.Connect.P99, msg, srv, cpu)
	}
}
