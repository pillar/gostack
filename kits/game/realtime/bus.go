package realtime

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// PostgresBus connects the hubs of several application instances through the
// PostgreSQL database they share (profile scale). See
// docs/adr/0005-realtime-scale.md for the design and docs/api/realtime.md for
// the client-visible semantics.
//
//   - Sequence authority: realtime_streams. Every message takes last_seq+1 in the
//     transaction that also stores it in realtime_messages (the shared replay
//     window, bounded per player by ReplayBuffer messages and ReplayMaxAge) and
//     issues pg_notify. Notifications are delivered in commit order, and commits
//     of one player's stream are serialised by its row, so every instance sees a
//     player's messages in seq order.
//   - Fan-out: each instance keeps one dedicated LISTEN session and delivers
//     notifications to its own connections. Gaps (missed notifications, payloads
//     too large to inline) are filled from realtime_messages.
//   - Degraded mode: while the LISTEN session is down or publishing failed
//     recently, Send/Broadcast deliver on this instance only, on local streams
//     with a fresh epoch (connections of the player are closed with 4012 first,
//     so no client mixes seqs of two authorities). The session reconnects with
//     backoff; on reconnect local streams are reset (4012) and bus streams catch
//     up from the table.
type PostgresBus struct {
	// DSN opens the LISTEN session; it must name the database of Module.DB.
	DSN string
	// ReconnectMin and ReconnectMax bound the reconnect and publish-retry
	// backoff (defaults 100ms and 5s, full jitter).
	ReconnectMin, ReconnectMax time.Duration
	// Timeout bounds each bus statement or transaction (default 5s).
	Timeout time.Duration
}

const (
	busChannel = "gostack_realtime"
	// maxNotify stays below PostgreSQL's 8000-byte NOTIFY payload limit.
	maxNotify = 7900
	batchMax  = 64
)

var errBusDown = errors.New("realtime: bus down")

// item is a queued Send or Broadcast (bid: fan-out of a remote broadcast).
type item struct {
	game, player, typ string
	data              []byte
	broadcast         bool
	bid               string
}

// note is a NOTIFY payload.
type note struct {
	K  string          `json:"k"` // "s": stored message, "b": broadcast
	G  string          `json:"g"`
	P  string          `json:"p,omitempty"`
	E  string          `json:"e,omitempty"`
	S  int64           `json:"s,omitempty"`
	B  json.RawMessage `json:"b,omitempty"` // client envelope; omitted when large (read from the table)
	ID string          `json:"id,omitempty"`
	T  string          `json:"t,omitempty"`
	D  json.RawMessage `json:"d,omitempty"`
}

type bus struct {
	h         *Hub
	o         PostgresBus
	q         chan item
	up        atomic.Bool
	pid       atomic.Uint32
	downUntil atomic.Int64 // unix nanos; publishing backs off after a failure
	fails     int          // consecutive publish failures (publisher goroutine)
	lastClean time.Time    // janitor goroutine
	ctx       context.Context
	cancel    context.CancelFunc
	pubDone   chan struct{}
	lisDone   chan struct{}
}

func newBus(h *Hub, o PostgresBus) *bus {
	if h.db == nil || !h.db.IsPostgres() {
		panic("realtime: PostgresBus requires a PostgreSQL store")
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = 100 * time.Millisecond
	}
	if o.ReconnectMax < o.ReconnectMin {
		o.ReconnectMax = max(5*time.Second, o.ReconnectMin)
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	b := &bus{h: h, o: o, q: make(chan item, h.opts.BusQueue), pubDone: make(chan struct{}), lisDone: make(chan struct{})}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	// Connect once before serving so a healthy start does not begin degraded.
	conn, err := b.connect()
	if err != nil {
		h.log.Warn("realtime bus: initial LISTEN failed; retrying in background", "err", err)
		h.count("realtime_bus_connect_failures_total", "")
	}
	go b.publisher()
	go b.listen(conn)
	return b
}

func (b *bus) healthy() bool {
	return b.up.Load() && time.Now().UnixNano() >= b.downUntil.Load()
}

func (b *bus) backoff(n int) time.Duration {
	c := min(b.o.ReconnectMin<<min(n, 16), b.o.ReconnectMax)
	return c/2 + time.Duration(rand.Int64N(int64(c/2)+1))
}

// enqueueLocked queues it for the publisher without blocking (h.mu held).
func (b *bus) enqueueLocked(it item) error {
	if b.h.closed {
		return ErrClosed
	}
	if it.broadcast && len(it.data)+len(it.typ)+len(it.game)+96 > maxNotify {
		return ErrTooLarge // a broadcast travels inside one NOTIFY payload
	}
	select {
	case b.q <- it:
		return nil
	default:
		b.h.count("realtime_send_dropped_total", "bus_queue")
		return ErrOverloaded
	}
}

func (b *bus) close(ctx context.Context) {
	b.h.mu.Lock() // closed is already set: no enqueue can race the close
	close(b.q)
	b.h.mu.Unlock()
	select {
	case <-b.pubDone:
	case <-ctx.Done():
	}
	b.cancel()
	<-b.pubDone
	<-b.lisDone
}

// ---- publisher ----

func (b *bus) publisher() {
	defer close(b.pubDone)
	batch := make([]item, 0, batchMax)
	for it := range b.q {
		batch = append(batch[:0], it)
	drain:
		for len(batch) < batchMax {
			select {
			case x, ok := <-b.q:
				if !ok {
					break drain
				}
				batch = append(batch, x)
			default:
				break drain
			}
		}
		b.publish(batch)
	}
}

func newEpoch() string {
	var r [6]byte
	_, _ = crand.Read(r[:])
	return "s" + hex.EncodeToString(r[:])
}

func (b *bus) publish(batch []item) {
	h := b.h
	if !b.healthy() {
		b.degrade(batch, "bus_down")
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, b.o.Timeout)
	defer cancel()
	keep := int64(h.opts.ReplayBuffer)
	var stored []key
	err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		stored = stored[:0]
		now := h.now().UnixMilli()
		notify := func(n note) error {
			p, _ := json.Marshal(n)
			if len(p) > maxNotify && n.B != nil {
				n.B = nil
				p, _ = json.Marshal(n)
			}
			_, err := tx.ExecContext(ctx, `SELECT pg_notify(?, ?)`, busChannel, string(p))
			return err
		}
		for _, it := range batch {
			if it.broadcast {
				if err := notify(note{K: "b", G: it.game, ID: newEpoch(), T: it.typ, D: it.data}); err != nil {
					return err
				}
				continue
			}
			var bid any
			if it.bid != "" {
				bid = it.bid
				var one int
				err := tx.QueryRowContext(ctx, `SELECT 1 FROM realtime_messages WHERE game_id=? AND player_id=? AND broadcast_id=?`,
					it.game, it.player, it.bid).Scan(&one)
				if err == nil {
					continue // another instance already delivered this broadcast to the player
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			var epoch string
			var seq int64
			err := tx.QueryRowContext(ctx, `INSERT INTO realtime_streams(game_id, player_id, epoch, last_seq, updated_at) VALUES(?,?,?,1,?)
				ON CONFLICT(game_id, player_id) DO UPDATE SET last_seq = realtime_streams.last_seq + 1, updated_at = excluded.updated_at
				RETURNING epoch, last_seq`, it.game, it.player, newEpoch(), now).Scan(&epoch, &seq)
			if err != nil {
				return err
			}
			env := envelope(it.typ, seq, it.data)
			if _, err := tx.ExecContext(ctx, `INSERT INTO realtime_messages(game_id, player_id, seq, broadcast_id, body, created_at) VALUES(?,?,?,?,?,?)`,
				it.game, it.player, seq, bid, string(env), now); err != nil {
				return err
			}
			if seq > keep {
				if _, err := tx.ExecContext(ctx, `DELETE FROM realtime_messages WHERE game_id=? AND player_id=? AND seq <= ?`,
					it.game, it.player, seq-keep); err != nil {
					return err
				}
			}
			if err := notify(note{K: "s", G: it.game, P: it.player, E: epoch, S: seq, B: env}); err != nil {
				return err
			}
			stored = append(stored, key{it.game, it.player})
		}
		return nil
	})
	if err != nil {
		b.fails++
		b.downUntil.Store(time.Now().Add(b.backoff(b.fails)).UnixNano())
		h.log.Warn("realtime bus: publish failed; delivering on this instance only", "err", err, "messages", len(batch))
		b.degrade(batch, "publish_failed")
		return
	}
	b.fails = 0
	h.mu.Lock()
	for _, k := range stored {
		h.count("realtime_bus_published_total", "")
		// The player's stream continues under the authority again.
		if st := h.streams[k]; st != nil && st.local {
			h.resetLocked(k, st, "bus_restored")
		}
	}
	h.mu.Unlock()
}

// degrade delivers batch on this instance only (local streams).
func (b *bus) degrade(batch []item, reason string) {
	h := b.h
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, it := range batch {
		h.count("realtime_bus_degraded_total", reason)
		if h.closed {
			continue
		}
		if it.broadcast {
			h.broadcastLocalLocked(it.game, it.typ, it.data)
			continue
		}
		if st, err := h.localStreamLocked(key{it.game, it.player}); err == nil {
			h.appendLocked(st, it.typ, it.data)
		}
	}
}

// ---- listener ----

func (b *bus) connect() (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(b.o.DSN)
	if err != nil {
		return nil, err
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = "gostack-realtime"
	ctx, cancel := context.WithTimeout(b.ctx, b.o.Timeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+busChannel); err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	return conn, nil
}

func (b *bus) listen(conn *pgx.Conn) {
	defer close(b.lisDone)
	h := b.h
	fails := 0
	for {
		if conn == nil {
			fails++
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(b.backoff(fails)):
			}
			var err error
			if conn, err = b.connect(); err != nil {
				if b.ctx.Err() != nil {
					return
				}
				h.count("realtime_bus_connect_failures_total", "")
				continue
			}
			h.count("realtime_bus_reconnects_total", "")
		}
		fails = 0
		b.connected(conn)
		err := b.receive(conn)
		b.up.Store(false)
		b.pid.Store(0)
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = conn.Close(cctx)
		cancel()
		conn = nil
		if b.ctx.Err() != nil {
			return
		}
		h.count("realtime_bus_disconnects_total", "")
		h.log.Warn("realtime bus: LISTEN session lost; delivering on this instance only until it reconnects", "err", err)
	}
}

// connected marks the bus up, resets local (degraded) streams and catches bus
// streams up on what was published while the session was down.
func (b *bus) connected(conn *pgx.Conn) {
	h := b.h
	b.pid.Store(conn.PgConn().PID())
	b.up.Store(true)
	type ref struct {
		k  key
		st *stream
	}
	var catch []ref
	h.mu.Lock()
	for k, st := range h.streams {
		switch {
		case st.syncing:
		case st.local:
			h.resetLocked(k, st, "bus_restored")
		default:
			catch = append(catch, ref{k, st})
		}
	}
	h.mu.Unlock()
	for _, r := range catch {
		b.fill(b.ctx, r.k, r.st, -1)
	}
}

func (b *bus) receive(conn *pgx.Conn) error {
	for {
		ctx, cancel := context.WithTimeout(b.ctx, 15*time.Second)
		n, err := conn.WaitForNotification(ctx)
		cancel()
		if err != nil {
			if b.ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && !conn.IsClosed() {
				// Idle: make sure the session is still alive.
				pctx, pcancel := context.WithTimeout(b.ctx, b.o.Timeout)
				err = conn.Ping(pctx)
				pcancel()
				if err == nil {
					continue
				}
			}
			return err
		}
		b.handle(n.Payload)
	}
}

func (b *bus) handle(payload string) {
	h := b.h
	var n note
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		h.count("realtime_bus_errors_total", "decode")
		return
	}
	switch n.K {
	case "s":
		k := key{n.G, n.P}
		h.mu.Lock()
		st := h.streams[k]
		switch {
		case st == nil || h.closed:
			h.mu.Unlock()
			return
		case st.syncing:
			if len(st.pending) < h.opts.ReplayBuffer {
				st.pending = append(st.pending, n)
			} else {
				st.missed = true
			}
			h.mu.Unlock()
			return
		case st.local:
			// Another instance (or our recovered publisher) advanced the
			// authority stream: the degraded local stream ends.
			h.resetLocked(k, st, "bus_restored")
			h.mu.Unlock()
			return
		}
		fill := b.applyLocked(k, st, n)
		h.mu.Unlock()
		if fill {
			b.fill(b.ctx, k, st, -1)
		}
	case "b":
		h.mu.Lock()
		for k, st := range h.streams {
			if k.game == n.G && len(st.conns) > 0 {
				// Each instance stores the broadcast for its own connected
				// players; broadcast_id makes it once per player.
				_ = b.enqueueLocked(item{game: k.game, player: k.player, typ: n.T, data: n.D, bid: n.ID})
			}
		}
		h.mu.Unlock()
	}
}

// applyLocked appends n when it is the next seq; it reports whether the stream
// must be filled from the table (gap or payload not inlined).
func (b *bus) applyLocked(k key, st *stream, n note) bool {
	if n.E != st.epoch {
		b.h.resetLocked(k, st, "epoch")
		return false
	}
	if n.S <= st.last {
		return false // already delivered (fill or resume read it from the table)
	}
	if n.S == st.last+1 && n.B != nil {
		b.h.pushLocked(st, n.S, []byte(n.B))
		return false
	}
	return true
}

// ---- table access ----

// sync reads (creating if needed) the authority stream of k for a new local
// bus stream st. When the bus is unhealthy or the read fails, st becomes a
// local stream (degraded).
func (b *bus) sync(_ context.Context, k key, st *stream) {
	h := b.h
	var epoch string
	var last int64
	err := errBusDown
	if b.healthy() {
		ctx, cancel := context.WithTimeout(b.ctx, b.o.Timeout)
		epoch, last, err = b.ensure(ctx, k)
		cancel()
	}
	fill := false
	h.mu.Lock()
	st.syncing = false
	if h.streams[k] == st {
		if err != nil {
			if !errors.Is(err, errBusDown) {
				h.log.Warn("realtime bus: stream sync failed; degraded to a local stream", "err", err)
			}
			h.count("realtime_bus_degraded_total", "sync")
			h.nstream++
			st.local, st.epoch, st.last = true, h.epoch+"."+strconv.FormatUint(h.nstream, 10), 0
		} else {
			st.epoch, st.last = epoch, last
			fill = st.missed
			for _, n := range st.pending {
				if h.streams[k] != st {
					break
				}
				fill = b.applyLocked(k, st, n) || fill
			}
		}
	}
	st.pending, st.missed = nil, false
	close(st.ready)
	h.mu.Unlock()
	if fill {
		b.fill(b.ctx, k, st, -1)
	}
}

func (b *bus) ensure(ctx context.Context, k key) (epoch string, last int64, err error) {
	db := b.h.db
	q := `SELECT epoch, last_seq FROM realtime_streams WHERE game_id=? AND player_id=?`
	err = db.Read.QueryRowContext(ctx, q, k.game, k.player).Scan(&epoch, &last)
	if !errors.Is(err, sql.ErrNoRows) {
		return epoch, last, err
	}
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO realtime_streams(game_id, player_id, epoch, last_seq, updated_at) VALUES(?,?,?,0,?)
			ON CONFLICT(game_id, player_id) DO NOTHING`, k.game, k.player, newEpoch(), b.h.now().UnixMilli()); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, q, k.game, k.player).Scan(&epoch, &last)
	})
	return epoch, last, err
}

type snapshot struct {
	epoch string // "" when the stream row does not exist
	last  int64
	rows  []entry // ascending
}

// load reads the authority stream of k and its retained messages after seq.
func (b *bus) load(ctx context.Context, k key, after int64) (s snapshot, err error) {
	h := b.h
	cutoff := h.now().Add(-h.opts.ReplayMaxAge).UnixMilli()
	err = h.db.ReadTx(ctx, func(tx *sql.Tx) error {
		s = snapshot{}
		err := tx.QueryRowContext(ctx, `SELECT epoch, last_seq FROM realtime_streams WHERE game_id=? AND player_id=?`,
			k.game, k.player).Scan(&s.epoch, &s.last)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT seq, body, created_at FROM realtime_messages
			WHERE game_id=? AND player_id=? AND seq > ? AND created_at >= ? ORDER BY seq LIMIT ?`,
			k.game, k.player, after, cutoff, 4*max(h.opts.ReplayBuffer, 64))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e entry
			var body string
			var at int64
			if err := rows.Scan(&e.seq, &body, &at); err != nil {
				return err
			}
			e.b, e.at = []byte(body), time.UnixMilli(at)
			s.rows = append(s.rows, e)
		}
		return rows.Err()
	})
	return s, err
}

// fill completes bus stream st from the table: messages after `after` (-1:
// after st.last). Live connections continue with what was missing; a gap that
// the table cannot close (trimmed or expired) resets the stream (4012) so
// clients resume and get resume_failed instead of silently skipping seqs.
// A load error resets the stream too, except for resume (after >= 0), which
// then answers from the local buffer.
func (b *bus) fill(ctx context.Context, k key, st *stream, after int64) {
	h := b.h
	h.mu.Lock()
	if h.streams[k] != st || st.local || st.syncing {
		h.mu.Unlock()
		return
	}
	forResume := after >= 0
	if !forResume {
		after = st.last
	}
	h.mu.Unlock()
	lctx, cancel := context.WithTimeout(ctx, b.o.Timeout)
	snap, err := b.load(lctx, k, after)
	cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams[k] != st || st.local || st.syncing {
		return
	}
	if err != nil {
		h.count("realtime_bus_errors_total", "load")
		if !forResume {
			h.resetLocked(k, st, "load_failed")
		}
		return
	}
	b.mergeLocked(k, st, snap)
}

// mergeLocked unions the table snapshot with st's buffer (both contiguous).
func (b *bus) mergeLocked(k key, st *stream, snap snapshot) {
	h := b.h
	if snap.epoch != st.epoch {
		h.resetLocked(k, st, "epoch")
		return
	}
	rows := snap.rows
	n := len(rows)
	if n > 0 && (rows[n-1].seq != snap.last || rows[n-1].seq-rows[0].seq != int64(n-1)) {
		h.resetLocked(k, st, "gap") // window larger than the read limit or not contiguous
		return
	}
	if snap.last > st.last && (n == 0 || rows[0].seq > st.last+1) {
		h.resetLocked(k, st, "gap")
		return
	}
	lo := st.last + 1
	if len(st.buf) > 0 {
		lo = st.buf[0].seq
	}
	var nb []entry
	if n > 0 && rows[0].seq < lo && rows[n-1].seq >= lo-1 {
		for _, r := range rows {
			if r.seq >= lo {
				break
			}
			nb = append(nb, r)
		}
	}
	if len(nb) == 0 && snap.last <= st.last {
		return
	}
	nb = append(nb, st.buf...)
	grew := false
	for _, r := range rows {
		if r.seq > st.last {
			nb = append(nb, r)
			grew = true
		}
	}
	st.buf = nb
	st.bytes = 0
	for _, e := range nb {
		st.bytes += len(e.b)
	}
	for len(st.buf) > h.opts.ReplayBuffer || (st.bytes > h.opts.ReplayMaxBytes && len(st.buf) > 1) {
		st.dropFront() // same bounds as live appends
	}
	h.count("realtime_bus_fills_total", "")
	if grew {
		st.last = snap.last
		st.touched = h.now()
		h.wakeLocked(st)
	}
}

// cleanup deletes replay rows older than ReplayMaxAge (janitor goroutine).
func (b *bus) cleanup() {
	h := b.h
	now := h.now()
	if now.Sub(b.lastClean) < max(h.opts.ReplayMaxAge/4, time.Second) {
		return
	}
	b.lastClean = now
	ctx, cancel := context.WithTimeout(b.ctx, b.o.Timeout)
	defer cancel()
	cutoff := now.Add(-h.opts.ReplayMaxAge).UnixMilli()
	err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM realtime_messages WHERE created_at < ?`, cutoff)
		return err
	})
	if err != nil && b.ctx.Err() == nil {
		h.count("realtime_bus_errors_total", "cleanup")
	}
}
