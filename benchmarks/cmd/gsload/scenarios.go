package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pillar/gostack/benchmarks/internal/benchdata"
)

// Budget is the PRD acceptance target a scenario is compared with.
type Budget struct {
	PRD        string  `json:"prd"`
	TargetRPS  float64 `json:"target_rps"`
	Percentile float64 `json:"percentile"` // 0.95 or 0.99
	LimitMs    float64 `json:"limit_ms"`
	MaxErrRate float64 `json:"max_error_rate"`
}

type weighted struct {
	op     string
	weight int
}

type scenario struct {
	name   string
	desc   string
	budget Budget
	mix    []weighted
}

// PRD §13 / §23 candidate targets (unverified until run on the 1C1G target).
var scenarios = []scenario{
	{"memory", "GET /healthz (no storage access)", Budget{"§13 内存 API", 1000, .99, 30, .001}, []weighted{{"healthz", 1}}},
	{"config_player", "GET /config (cached doc + game row read) 50% / GET /me (session + player point reads) 50%", Budget{"§23 配置与玩家读取", 300, .95, 30, .001}, []weighted{{"config", 1}, {"me", 1}}},
	{"pointread", "GET /saves/profile: session lookup + PK read of a 1 KiB save", Budget{"§13 索引点查", 500, .99, 50, .001}, []weighted{{"save_get_1k", 1}}},
	{"mixed9010", "90% GET /saves/profile, 10% PUT /saves/profile (1 KiB, FULL)", Budget{"§13 90/10 混合", 300, .99, 100, .001}, []weighted{{"save_get_1k", 9}, {"save_put_1k", 1}}},
	{"fullwrite", "50% PUT /saves/profile (1 KiB) + 50% POST /rewards/{id}/claim, FULL sync, Idempotency-Key", Budget{"§13 FULL 同步写", 100, .99, 150, .001}, []weighted{{"save_put_1k", 1}, {"claim", 1}}},
	{"board", "GET /boards/main?limit=100 (anonymous Top-100, 100k-row current season, 1M-row table)", Budget{"§23 Top 100 榜单", 200, .95, 20, .001}, []weighted{{"board_top100", 1}}},
	{"save8k", "PUT /saves/main with an 8 KiB payload (FULL, history 10, Idempotency-Key)", Budget{"§23 8KiB 存档写入", 50, .95, 50, .001}, []weighted{{"save_put_8k", 1}}},
	{"economy", "POST /rewards/bench_coin/claim (FULL); 2% replays of an earlier Idempotency-Key", Budget{"§23 经济事务 FULL", 50, .99, 150, .001}, []weighted{{"claim", 1}}},
	{"gamemix", "read 70% (config 20, me 15, save8k get 15, board 10, wallet 10) / save 10% / economy 5% / analytics 15% (20 events)", Budget{"§23 混合负载", 200, .99, 150, .001},
		[]weighted{{"config", 20}, {"me", 15}, {"save_get_8k", 15}, {"board_top100", 10}, {"wallet", 10}, {"save_put_8k", 10}, {"claim", 5}, {"events20", 15}}},
}

func findScenario(name string) (scenario, bool) {
	for _, s := range scenarios {
		if s.name == name {
			return s, true
		}
	}
	return scenario{}, false
}

// outcome of one request.
type outcome struct {
	op       string
	status   int
	kind     string // "" ok | "expected:<code>" | error kind
	errMsg   string // transport error text (first one per kind is kept in the result)
	replayOK *bool
}

// client holds shared state for operations.
type client struct {
	base    string
	hc      *http.Client
	seed    uint64
	players int
	runID   string
	state   *revState
	saveCur atomic.Int64
	econCur atomic.Int64

	mu      sync.Mutex
	claims  []claimRec // recent successful claims for replay checks
	replays struct{ ok, mismatch atomic.Int64 }
}

type claimRec struct {
	player  int
	key     string
	claimID string
}

// revState tracks the expected revision of saves the generator wrote; seeded saves are revision 1.
type revState struct {
	mu   sync.Mutex
	Revs map[string]int64 `json:"revs"`
	Save int64            `json:"save_cursor"`
	Econ int64            `json:"econ_cursor"`
}

func (s *revState) get(slot string, p int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.Revs[slot+":"+strconv.Itoa(p)]; ok {
		return r
	}
	return 1
}

func (s *revState) set(slot string, p int, r int64) {
	s.mu.Lock()
	s.Revs[slot+":"+strconv.Itoa(p)] = r
	s.mu.Unlock()
}

func (c *client) url(p string) string {
	return c.base + "/v1/games/" + benchdata.GameID + p
}

func (c *client) do(ctx context.Context, method, url string, player int, body []byte, hdr map[string]string) (int, []byte, http.Header, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if player >= 0 {
		req.Header.Set("Authorization", "Bearer "+benchdata.Token(c.seed, player))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header, err
}

type errBody struct {
	Error struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// expectedCodes are business rejections that are correct server behaviour and
// excluded from the error rate (PRD §13 "不含预期业务拒绝").
var expectedCodes = map[string]bool{"already_claimed": true, "insufficient_funds": true}

func classify(status int, b []byte, err error) (string, errBody) {
	var eb errBody
	if err != nil {
		if strings.Contains(err.Error(), "Timeout") || strings.Contains(err.Error(), "deadline") {
			return "timeout", eb
		}
		return "transport", eb
	}
	if status < 400 {
		return "", eb
	}
	_ = json.Unmarshal(b, &eb)
	if expectedCodes[eb.Error.Code] {
		return "expected:" + eb.Error.Code, eb
	}
	return fmt.Sprintf("http_%d:%s", status, eb.Error.Code), eb
}

// run executes op with randomness derived from r.
func (c *client) run(ctx context.Context, op string, r *rand.Rand) outcome {
	o := outcome{op: op}
	var st int
	var b []byte
	var h http.Header
	var err error
	rp := r.IntN(c.players)
	switch op {
	case "healthz":
		st, b, _, err = c.do(ctx, "GET", c.base+"/healthz", -1, nil, nil)
	case "config":
		st, b, _, err = c.do(ctx, "GET", c.url("/config"), -1, nil, nil)
	case "me":
		st, b, _, err = c.do(ctx, "GET", c.url("/me"), rp, nil, nil)
	case "wallet":
		st, b, _, err = c.do(ctx, "GET", c.url("/wallet"), rp, nil, nil)
	case "save_get_1k":
		st, b, _, err = c.do(ctx, "GET", c.url("/saves/"+benchdata.SlotSmall), rp, nil, nil)
	case "save_get_8k":
		st, b, _, err = c.do(ctx, "GET", c.url("/saves/"+benchdata.SlotBig), rp, nil, nil)
	case "board_top100":
		st, b, _, err = c.do(ctx, "GET", c.url("/boards/"+benchdata.Board+"?limit=100"), -1, nil, nil)
	case "save_put_1k", "save_put_8k":
		slot, n := benchdata.SlotSmall, benchdata.SmallBytes
		if op == "save_put_8k" {
			slot, n = benchdata.SlotBig, benchdata.BigBytes
		}
		p := int(c.saveCur.Add(1) % int64(c.players))
		rev := c.state.get(slot, p)
		body := fmt.Appendf(nil, `{"expected_revision":%d,"schema_version":1,"payload":%s}`, rev, benchdata.Payload(n, c.seed, p, rev+1))
		key := fmt.Sprintf("sv-%s-%d-%d", c.runID, p, rev)
		st, b, _, err = c.do(ctx, "PUT", c.url("/saves/"+slot), p, body, map[string]string{"Idempotency-Key": key})
		if err == nil && st == 200 {
			var d struct {
				Revision int64 `json:"revision"`
			}
			if json.Unmarshal(b, &d) == nil && d.Revision > 0 {
				c.state.set(slot, p, d.Revision)
			}
		} else if err == nil && st == 409 {
			_, eb := classify(st, b, nil)
			if cur, ok := eb.Error.Details["current_revision"].(float64); ok && cur > 0 {
				c.state.set(slot, p, int64(cur))
			}
		}
	case "claim":
		// 2% of claims replay an earlier key: must return the same claim (no double grant).
		c.mu.Lock()
		var rec *claimRec
		if len(c.claims) > 0 && r.IntN(50) == 0 {
			x := c.claims[r.IntN(len(c.claims))]
			rec = &x
		}
		c.mu.Unlock()
		if rec != nil {
			o.op = "claim_replay"
			st, b, h, err = c.do(ctx, "POST", c.url("/rewards/"+benchdata.Reward+"/claim"), rec.player, []byte("{}"), map[string]string{"Idempotency-Key": rec.key})
			if err == nil && st == 200 {
				var d struct {
					ClaimID string `json:"claim_id"`
				}
				_ = json.Unmarshal(b, &d)
				ok := d.ClaimID == rec.claimID && h.Get("Idempotent-Replayed") == "true"
				o.replayOK = &ok
				if ok {
					c.replays.ok.Add(1)
				} else {
					c.replays.mismatch.Add(1)
				}
			}
			break
		}
		p := int(c.econCur.Add(1) % int64(c.players))
		key := fmt.Sprintf("cl-%s-%d", c.runID, r.Uint64())
		st, b, h, err = c.do(ctx, "POST", c.url("/rewards/"+benchdata.Reward+"/claim"), p, []byte("{}"), map[string]string{"Idempotency-Key": key})
		if err == nil && st == 200 {
			var d struct {
				ClaimID string `json:"claim_id"`
			}
			if json.Unmarshal(b, &d) == nil && d.ClaimID != "" {
				c.mu.Lock()
				if len(c.claims) < 4096 {
					c.claims = append(c.claims, claimRec{p, key, d.ClaimID})
				} else {
					c.claims[r.IntN(len(c.claims))] = claimRec{p, key, d.ClaimID}
				}
				c.mu.Unlock()
			}
		}
	case "events20":
		var sb strings.Builder
		sb.WriteString(`{"events":[`)
		u := r.Uint64()
		for j := range 20 {
			if j > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"event_id":"e-%s-%x-%d","event_name":"level_end","session_id":"s-%d","platform":"web","client_version":"1.0.0","schema_version":1,"client_time":%d,"properties":{"level":%d,"result":"win","duration_ms":%d}}`,
				c.runID, u, j, rp, 1764000000000+int64(j), j+1, 30000+j)
		}
		sb.WriteString(`]}`)
		st, b, _, err = c.do(ctx, "POST", c.url("/events/batch"), rp, []byte(sb.String()), nil)
	default:
		panic("unknown op " + op)
	}
	o.status = st
	o.kind, _ = classify(st, b, err)
	if err != nil {
		o.errMsg = err.Error()
	}
	return o
}
