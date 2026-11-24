package game

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/ratelimit"
)

// Analytics ingests best-effort events (P2 data) into its own SQLite file
// through a bounded in-memory buffer. When the buffer is full events are dropped
// and counted; ingestion never touches the main database (G07, G-A06).
type Analytics struct {
	DB  *sqlite.DB
	Log *slog.Logger

	ch       chan analyticsRow
	accepted atomic.Int64
	dropped  atomic.Int64
	written  atomic.Int64
	done     chan struct{}
}

type analyticsRow struct {
	GameID, PlayerID, EventID, SessionID, Name, Platform, ClientVersion string
	SchemaVersion, ClientTime, ServerTime                               int64
	Properties                                                          []byte
}

func NewAnalytics(db *sqlite.DB, buffer int, log *slog.Logger) *Analytics {
	return &Analytics{DB: db, Log: log, ch: make(chan analyticsRow, buffer), done: make(chan struct{})}
}

// Stats returns accepted / dropped / written counters and current buffer depth.
func (a *Analytics) Stats() (accepted, dropped, written int64, depth int) {
	return a.accepted.Load(), a.dropped.Load(), a.written.Load(), len(a.ch)
}

func (a *Analytics) offer(r analyticsRow) bool {
	select {
	case a.ch <- r:
		a.accepted.Add(1)
		return true
	default:
		a.dropped.Add(1)
		return false
	}
}

// Run flushes batches until ctx is cancelled, then drains what is buffered
// (bounded by drainTimeout) and returns.
func (a *Analytics) Run(ctx context.Context, drainTimeout time.Duration) {
	defer close(a.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	batch := make([]analyticsRow, 0, 500)
	flush := func(parent context.Context) {
		if len(batch) == 0 {
			return
		}
		// A batch already taken from the buffer is written even if shutdown starts meanwhile.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer cancel()
		err := a.DB.WriteTx(fctx, func(tx *sql.Tx) error {
			st, err := tx.PrepareContext(fctx, `INSERT OR IGNORE INTO analytics_events(event_id,game_id,player_id,session_id,event_name,schema_version,platform,client_version,client_time,server_time,properties)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
			if err != nil {
				return err
			}
			defer st.Close()
			for _, r := range batch {
				var pid any
				if r.PlayerID != "" {
					pid = r.PlayerID
				}
				if _, err := st.ExecContext(fctx, r.EventID, r.GameID, pid, r.SessionID, r.Name, r.SchemaVersion, r.Platform, r.ClientVersion, r.ClientTime, r.ServerTime, string(r.Properties)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			a.dropped.Add(int64(len(batch)))
			a.Log.Warn("analytics flush failed; batch dropped", "events", len(batch), "err", err)
		} else {
			a.written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	for {
		select {
		case r := <-a.ch:
			batch = append(batch, r)
			if len(batch) == cap(batch) {
				flush(ctx)
			}
		case <-tick.C:
			flush(ctx)
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			defer cancel()
		drain:
			for {
				select {
				case r := <-a.ch:
					batch = append(batch, r)
					if len(batch) == cap(batch) {
						flush(dctx)
					}
				default:
					break drain
				}
			}
			flush(dctx)
			if n := len(a.ch); n > 0 {
				a.Log.Warn("analytics events left unflushed at shutdown", "events", n)
			}
			return
		}
	}
}

// Done is closed when Run returns.
func (a *Analytics) Done() <-chan struct{} { return a.done }

// PurgeOlderThan deletes events past retention.
func (a *Analytics) PurgeOlderThan(ctx context.Context, d time.Duration) (int64, error) {
	var n int64
	err := a.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM analytics_events WHERE server_time<?`, time.Now().Add(-d).UnixMilli())
		if err == nil {
			n, _ = res.RowsAffected()
		}
		return err
	})
	return n, err
}

type eventIn struct {
	EventID       string          `json:"event_id"`
	EventName     string          `json:"event_name"`
	SessionID     string          `json:"session_id"`
	ClientTime    int64           `json:"client_time"`
	ClientVersion string          `json:"client_version"`
	Platform      string          `json:"platform"`
	SchemaVersion int64           `json:"schema_version"`
	Properties    json.RawMessage `json:"properties"`
}

func validToken(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func (k *Kit) eventsBatch(w http.ResponseWriter, r *http.Request) error {
	g := gameFrom(r.Context())
	if err := k.rateLimit(k.eventLimit, "events:"+g.ID+":"+ratelimit.IPKey(k.ClientIP.Of(r))); err != nil {
		return err
	}
	body, err := readBody(r, 64<<10)
	if err != nil {
		return err
	}
	var req struct {
		Events []eventIn `json:"events"`
	}
	if err := decodeStrict(body, &req); err != nil {
		return err
	}
	if len(req.Events) == 0 || len(req.Events) > 100 {
		return httpx.BadRequest("events must contain 1-100 items")
	}
	var pid string
	if s := sessionFrom(r.Context()); s != nil {
		pid = s.PlayerID
	}
	now := k.Now().UnixMilli()
	accepted, dropped := 0, 0
	for i, e := range req.Events {
		if !validToken(e.EventID, 64) || !validToken(e.EventName, 64) || !validToken(e.SessionID, 64) ||
			len(e.ClientVersion) > 32 || len(e.Platform) > 16 || len(e.Properties) > 4096 {
			return httpx.BadRequest("invalid event at index " + strconv.Itoa(i))
		}
		props := e.Properties
		if len(props) == 0 || string(props) == "null" {
			props = []byte("{}")
		} else if props[0] != '{' {
			return httpx.BadRequest("properties must be an object at index " + strconv.Itoa(i))
		}
		row := analyticsRow{GameID: g.ID, PlayerID: pid, EventID: e.EventID, SessionID: e.SessionID, Name: e.EventName,
			Platform: e.Platform, ClientVersion: e.ClientVersion, SchemaVersion: e.SchemaVersion, ClientTime: e.ClientTime, ServerTime: now, Properties: props}
		if k.Analytics.offer(row) {
			accepted++
		} else {
			dropped++
		}
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]int{"accepted": accepted, "dropped": dropped})
	return nil
}
