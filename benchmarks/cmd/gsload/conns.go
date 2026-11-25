package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/pillar/gostack/benchmarks/internal/benchdata"
	"github.com/pillar/gostack/benchmarks/internal/hist"
)

// ConnResult reports a long-connection run.
type ConnResult struct {
	Label        string           `json:"label"`
	Scenario     string           `json:"scenario"`
	Description  string           `json:"description"`
	Target       int              `json:"target_connections"`
	Opened       int64            `json:"opened"`
	OpenErrors   map[string]int64 `json:"open_errors"`
	HoldS        float64          `json:"hold_s"`
	AliveAtEnd   int64            `json:"alive_at_end"`
	DroppedEarly int64            `json:"dropped_during_hold"`
	Connect      hist.Summary     `json:"connect_latency"`
	MsgInterval  float64          `json:"msg_interval_s,omitempty"`
	MsgSent      int64            `json:"messages_sent,omitempty"`
	MsgRTT       *hist.Summary    `json:"message_rtt,omitempty"`
	MsgErrors    int64            `json:"message_errors,omitempty"`
	Server       *ServerSample    `json:"server,omitempty"`
	ClientGo     string           `json:"client_go"`
	Notes        []string         `json:"notes,omitempty"`
}

// runWS opens conns authenticated WebSocket connections to the realtime gateway
// (distinct players, Authorization header) and holds them for -duration. With
// -ws-msg-interval each connection sends {"type":"ping"} at that interval and the
// round trip to the server's pong is recorded.
func runWS(f flags) *ConnResult {
	res := &ConnResult{Label: f.label, Scenario: "ws", Target: f.conns, OpenErrors: map[string]int64{}, HoldS: f.duration.Seconds(),
		ClientGo: runtime.Version(), MsgInterval: f.wsInterval.Seconds()}
	res.Description = "idle authenticated WebSocket connections (realtime module); client answers server pings"
	if f.wsInterval > 0 {
		res.Description = fmt.Sprintf("active WebSocket connections, one ping message per connection every %s", f.wsInterval)
	}
	wsURL := "ws" + strings.TrimPrefix(strings.TrimRight(f.url, "/"), "http") + "/v1/games/" + benchdata.GameID + "/realtime"
	connectH, rttH := hist.New(), hist.New()
	var mu sync.Mutex
	var alive, dropped, sent, msgErr atomic.Int64
	holdCtx, cancelHold := context.WithCancel(context.Background())
	var conns []*websocket.Conn
	var wg sync.WaitGroup
	hc := &http.Client{Transport: newTransport()}
	interval := time.Duration(float64(time.Second) / f.connRate)
	start := time.Now()
	for i := 0; i < f.conns; i++ {
		if d := time.Until(start.Add(time.Duration(i) * interval)); d > 0 {
			time.Sleep(d)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := (i * 7) % f.players // distinct players (max_per_player is 2)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t0 := time.Now()
			c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: hc,
				HTTPHeader: http.Header{"Authorization": {"Bearer " + benchdata.Token(f.seed, p)}}})
			if err == nil {
				// The server sends welcome after authenticating.
				_, msg, rerr := c.Read(ctx)
				if rerr != nil || !strings.Contains(string(msg), `"welcome"`) {
					err = fmt.Errorf("no welcome: %v %s", rerr, msg)
					c.CloseNow()
				}
			}
			cancel()
			if err != nil {
				k := "dial"
				if resp != nil {
					k = fmt.Sprintf("http_%d", resp.StatusCode)
				}
				mu.Lock()
				res.OpenErrors[k]++
				mu.Unlock()
				return
			}
			connectH.Record(time.Since(t0))
			alive.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			pongs := make(chan time.Time, 4)
			go func() { // reader: processes control frames (server pings) and pongs
				for {
					_, msg, err := c.Read(holdCtx)
					if err != nil {
						if holdCtx.Err() == nil {
							alive.Add(-1)
							dropped.Add(1)
						}
						return
					}
					if strings.Contains(string(msg), `"pong"`) {
						select {
						case pongs <- time.Now():
						default:
						}
					}
				}
			}()
			if f.wsInterval <= 0 {
				return
			}
			go func() {
				r := rand.New(rand.NewPCG(f.seed, uint64(i)))
				timer := time.NewTimer(time.Duration(r.Int64N(int64(f.wsInterval))))
				defer timer.Stop()
				for {
					select {
					case <-holdCtx.Done():
						return
					case <-timer.C:
					}
					t := time.Now()
					wctx, wc := context.WithTimeout(holdCtx, 5*time.Second)
					err := c.Write(wctx, websocket.MessageText, []byte(`{"type":"ping"}`))
					if err == nil {
						sent.Add(1)
						select {
						case at := <-pongs:
							rttH.Record(at.Sub(t))
						case <-wctx.Done():
							err = wctx.Err()
						}
					}
					wc()
					if err != nil && holdCtx.Err() == nil {
						msgErr.Add(1)
					}
					timer.Reset(f.wsInterval)
				}
			}()
		}(i)
	}
	wg.Wait()
	res.Opened = alive.Load()
	log.Printf("ws: opened %d/%d in %s; holding %s", res.Opened, f.conns, time.Since(start).Round(time.Millisecond), f.duration)
	sp := startSampler(f.pid)
	time.Sleep(f.duration)
	res.Server = sp.finish()
	res.AliveAtEnd, res.DroppedEarly = alive.Load(), dropped.Load()
	cancelHold()
	for _, c := range conns {
		c.Close(websocket.StatusNormalClosure, "")
	}
	res.Connect = connectH.Summary()
	if f.wsInterval > 0 {
		s := rttH.Summary()
		res.MsgRTT, res.MsgSent, res.MsgErrors = &s, sent.Load(), msgErr.Load()
	}
	b, _ := json.Marshal(res.OpenErrors)
	log.Printf("ws: alive at end %d, dropped %d, open errors %s", res.AliveAtEnd, res.DroppedEarly, b)
	return res
}

// runIdleHTTP opens conns HTTP/1.1 keep-alive connections, sends one GET
// /healthz on each, holds them idle for -duration, then checks how many the
// server kept open by sending a second request on each.
func runIdleHTTP(f flags) *ConnResult {
	res := &ConnResult{Label: f.label, Scenario: "idlehttp", Target: f.conns, OpenErrors: map[string]int64{}, HoldS: f.duration.Seconds(),
		ClientGo: runtime.Version(), Description: "idle HTTP/1.1 keep-alive connections after one GET /healthz each"}
	host := strings.TrimPrefix(strings.TrimPrefix(f.url, "http://"), "https://")
	host = strings.TrimRight(host, "/")
	connectH := hist.New()
	type cw struct {
		c  net.Conn
		br *bufio.Reader
	}
	var conns []cw
	var mu sync.Mutex
	var wg sync.WaitGroup
	interval := time.Duration(float64(time.Second) / f.connRate)
	start := time.Now()
	get := func(c net.Conn, br *bufio.Reader) error {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(c, "GET /healthz HTTP/1.1\r\nHost: %s\r\n\r\n", host); err != nil {
			return err
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return err
		}
		buf := make([]byte, 512)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				break
			}
		}
		resp.Body.Close()
		c.SetDeadline(time.Time{})
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}
	for i := 0; i < f.conns; i++ {
		if d := time.Until(start.Add(time.Duration(i) * interval)); d > 0 {
			time.Sleep(d)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			c, err := net.DialTimeout("tcp", host, 5*time.Second)
			if err == nil {
				br := bufio.NewReader(c)
				if err = get(c, br); err == nil {
					connectH.Record(time.Since(t0))
					mu.Lock()
					conns = append(conns, cw{c, br})
					mu.Unlock()
					return
				}
				c.Close()
			}
			mu.Lock()
			res.OpenErrors[strings.SplitN(err.Error(), ":", 2)[0]]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	res.Opened = int64(len(conns))
	log.Printf("idlehttp: opened %d/%d; holding %s", res.Opened, f.conns, f.duration)
	sp := startSampler(f.pid)
	time.Sleep(f.duration)
	res.Server = sp.finish()
	var aliveN atomic.Int64
	var wg2 sync.WaitGroup
	for _, x := range conns {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if get(x.c, x.br) == nil {
				aliveN.Add(1)
			}
			x.c.Close()
		}()
	}
	wg2.Wait()
	res.AliveAtEnd = aliveN.Load()
	res.DroppedEarly = res.Opened - res.AliveAtEnd
	res.Connect = connectH.Summary()
	res.Notes = append(res.Notes, "server http.Server IdleTimeout is 120s (app/app.go); holds longer than that are expected to be closed by the server")
	log.Printf("idlehttp: alive at end %d/%d", res.AliveAtEnd, res.Opened)
	return res
}
