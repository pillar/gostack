package realtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/observability"
)

// Errors returned by Send and Broadcast.
var (
	ErrClosed      = errors.New("realtime: hub closed")
	ErrTooLarge    = errors.New("realtime: payload too large")
	ErrInvalidType = errors.New("realtime: invalid or reserved message type")
	ErrOverloaded  = errors.New("realtime: too many player streams buffered")
)

var typePattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,63}$`)

// reserved are protocol message types; neither side may use them for app messages.
var reserved = map[string]bool{
	"auth": true, "ping": true, "pong": true, "ack": true, "resume": true, "resumed": true,
	"resume_failed": true, "welcome": true, "error": true, "ok": true,
}

func validType(t string) bool { return typePattern.MatchString(t) && !reserved[t] }

type key struct{ game, player string }

type entry struct {
	seq int64
	at  time.Time
	b   []byte
}

// stream is a player's ordered message sequence: the last assigned seq, the
// bounded replay buffer (ascending, contiguous seqs) and attached connections.
//
// local streams assign seqs in this process (always without a bus; under the
// bus only while degraded). Bus streams mirror the PostgreSQL stream: seqs come
// from realtime_streams and messages arrive through the bus.
type stream struct {
	epoch   string
	last    int64
	buf     []entry
	bytes   int
	conns   []*conn // attach order, oldest first
	touched time.Time

	local   bool
	syncing bool          // bus stream whose epoch/last are being read
	ready   chan struct{} // closed when syncing ends
	started bool          // a goroutine runs the sync
	pending []note        // notes received while syncing
	missed  bool          // pending overflowed: fill from the table after sync
}

func (st *stream) dropFront() {
	st.bytes -= len(st.buf[0].b)
	st.buf[0] = entry{}
	st.buf = st.buf[1:]
}

// Hub owns all realtime connections and player streams of this process.
type Hub struct {
	opts      Options
	db        *store.DB
	now       func() time.Time
	metrics   *observability.Registry
	log       *slog.Logger
	onMessage Handler
	epoch     string

	base   context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	jdone  chan struct{}

	bus *bus // nil: in-process only (profile lite)

	mu      sync.Mutex
	streams map[key]*stream
	conns   map[*conn]struct{}
	slots   int // reserved connections (pending + registered)
	nstream uint64
	closed  bool
	wg      sync.WaitGroup
}

func newHub(o Options, db *store.DB, now func() time.Time, m *observability.Registry, log *slog.Logger, h Handler, pb *PostgresBus) *Hub {
	var b [6]byte
	_, _ = rand.Read(b[:])
	hub := &Hub{opts: o, db: db, now: now, metrics: m, log: log, onMessage: h, epoch: hex.EncodeToString(b[:]),
		stop: make(chan struct{}), jdone: make(chan struct{}),
		streams: map[key]*stream{}, conns: map[*conn]struct{}{}}
	hub.base, hub.cancel = context.WithCancel(context.Background())
	if m != nil {
		m.Gauge("realtime_connections", func() float64 { return float64(hub.Stats().Connections) })
		m.Gauge("realtime_streams", func() float64 { return float64(hub.Stats().Streams) })
	}
	if pb != nil {
		hub.bus = newBus(hub, *pb)
		if m != nil {
			m.Gauge("realtime_bus_up", func() float64 {
				if hub.bus.healthy() {
					return 1
				}
				return 0
			})
		}
	}
	go hub.janitor()
	return hub
}

func (h *Hub) count(name, reason string) {
	if h.metrics != nil {
		labels := ""
		if reason != "" {
			labels = `reason="` + reason + `"`
		}
		h.metrics.Counter(name, labels).Add(1)
	}
}

// Stats is a point-in-time view of the hub.
type Stats struct {
	Connections int  // reserved connection slots (incl. not yet authenticated)
	Streams     int  // player streams held in memory
	Bus         bool // a bus is configured
	// BusUp: the LISTEN session is connected and publishing is not backing off
	// after a failure. While false, Send/Broadcast deliver on this instance only.
	BusUp bool
	// BusPID is the PostgreSQL backend pid of the LISTEN session (0 when down).
	BusPID uint32
}

func (h *Hub) Stats() Stats {
	h.mu.Lock()
	s := Stats{Connections: h.slots, Streams: len(h.streams)}
	h.mu.Unlock()
	if h.bus != nil {
		s.Bus, s.BusUp, s.BusPID = true, h.bus.healthy(), h.bus.pid.Load()
	}
	return s
}

func (h *Hub) reserve() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.slots >= h.opts.MaxConnections {
		return false
	}
	h.slots++
	h.wg.Add(1)
	return true
}

func (h *Hub) release() {
	h.mu.Lock()
	h.slots--
	h.mu.Unlock()
	h.wg.Done()
}

// register tracks c for shutdown; false when the hub is closing.
func (h *Hub) register(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

func (h *Hub) newStreamLocked(k key) *stream {
	h.nstream++
	st := &stream{epoch: h.epoch + "." + strconv.FormatUint(h.nstream, 10), touched: h.now(), local: true}
	h.streams[k] = st
	return st
}

// resetLocked drops a stream: its connections are closed with 4012 so their
// clients reconnect, get a fresh welcome and resume (or resync over HTTP).
func (h *Hub) resetLocked(k key, st *stream, reason string) {
	for _, c := range st.conns {
		c.st = nil
		c.close(CodeStreamReset, "stream reset", "stream_reset")
	}
	st.conns = nil
	if h.streams[k] == st {
		delete(h.streams, k)
	}
	h.count("realtime_stream_resets_total", reason)
}

// localStreamLocked returns the player's local stream, replacing a bus stream
// (degraded delivery) or creating one within MaxStreams.
func (h *Hub) localStreamLocked(k key) (*stream, error) {
	st := h.streams[k]
	if st != nil && !st.local {
		h.resetLocked(k, st, "degraded")
		st = nil
	}
	if st == nil {
		if len(h.streams) >= h.opts.MaxStreams {
			h.count("realtime_send_dropped_total", "max_streams")
			return nil, ErrOverloaded
		}
		st = h.newStreamLocked(k)
	}
	return st, nil
}

// attach makes c a live receiver of the player's stream. When the player is at
// MaxPerPlayer the oldest connection is replaced (closed with 4009): a client
// that reconnected after a network change must not be locked out by its own
// half-open connection until the pong timeout. Under the bus a new stream is
// first synchronised with PostgreSQL (epoch and last seq).
func (h *Hub) attach(ctx context.Context, c *conn) (epoch string, last int64, ok bool) {
	k := key{c.game, c.player}
	h.mu.Lock()
	for {
		if h.closed {
			h.mu.Unlock()
			return "", 0, false
		}
		st := h.streams[k]
		if st == nil {
			if h.bus == nil {
				st = h.newStreamLocked(k)
			} else {
				st = &stream{syncing: true, ready: make(chan struct{}), touched: h.now()}
				h.streams[k] = st
			}
		}
		if st.syncing {
			ready, run := st.ready, !st.started
			st.started = true
			h.mu.Unlock()
			if run {
				h.bus.sync(ctx, k, st)
			} else {
				select {
				case <-ready:
				case <-ctx.Done():
					return "", 0, false
				}
			}
			h.mu.Lock()
			continue
		}
		for len(st.conns) >= h.opts.MaxPerPlayer {
			old := st.conns[0]
			st.conns = st.conns[1:]
			old.st = nil
			old.close(CodeReplaced, "replaced by a newer connection", "replaced")
		}
		c.st = st
		c.cursor, c.liveFrom = st.last, st.last
		st.conns = append(st.conns, c)
		h.mu.Unlock()
		return st.epoch, st.last, true
	}
}

// detach removes c from the hub and frees its slot.
func (h *Hub) detach(c *conn) {
	h.mu.Lock()
	if st := c.st; st != nil {
		for i, o := range st.conns {
			if o == c {
				st.conns = append(st.conns[:i], st.conns[i+1:]...)
				break
			}
		}
		st.touched = h.now()
		c.st = nil
	}
	delete(h.conns, c)
	h.slots--
	h.mu.Unlock()
	h.wg.Done()
}

func encodePayload(o Options, typ string, payload any) ([]byte, error) {
	if !validType(typ) {
		return nil, ErrInvalidType
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(data) > o.MaxPayloadBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Send queues a message for the player without blocking. It gets the player's
// next seq and is kept in the replay buffer (within its limits) even when the
// player is offline, so a reconnecting client can resume. Connected clients
// that fall more than SendQueue messages behind are closed with 4008.
//
// Under the bus the message is queued for the publisher (ErrOverloaded when
// the queue is full); seq is assigned by PostgreSQL and the message reaches
// the player's connections on every instance (see docs/api/realtime.md).
func (h *Hub) Send(gameID, playerID, typ string, payload any) error {
	data, err := encodePayload(h.opts, typ, payload)
	if err != nil {
		h.count("realtime_send_dropped_total", "invalid")
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	if h.bus != nil {
		return h.bus.enqueueLocked(item{game: gameID, player: playerID, typ: typ, data: data})
	}
	st, err := h.localStreamLocked(key{gameID, playerID})
	if err != nil {
		return err
	}
	h.appendLocked(st, typ, data)
	return nil
}

// Broadcast sends to every player of gameID that currently has a connection
// (offline players are skipped; each recipient gets its own seq). Without a
// bus that means connections on this instance; under the bus every instance
// fans the broadcast out to its own connected players. Work is bounded by
// MaxConnections per instance. It returns the number of players connected to
// this instance at call time.
func (h *Hub) Broadcast(gameID, typ string, payload any) (int, error) {
	data, err := encodePayload(h.opts, typ, payload)
	if err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, ErrClosed
	}
	n := 0
	for k, st := range h.streams {
		if k.game == gameID && len(st.conns) > 0 {
			n++
			if h.bus == nil {
				h.appendLocked(st, typ, data)
			}
		}
	}
	if h.bus != nil {
		if err := h.bus.enqueueLocked(item{game: gameID, typ: typ, data: data, broadcast: true}); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// broadcastLocalLocked delivers a broadcast on local streams to this
// instance's connected players (degraded bus).
func (h *Hub) broadcastLocalLocked(gameID, typ string, data []byte) {
	var keys []key
	for k, st := range h.streams {
		if k.game == gameID && len(st.conns) > 0 {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if st, err := h.localStreamLocked(k); err == nil {
			h.appendLocked(st, typ, data)
		}
	}
}

func envelope(typ string, seq int64, data []byte) []byte {
	b := make([]byte, 0, len(typ)+len(data)+40)
	b = append(b, `{"type":"`...)
	b = append(b, typ...)
	b = append(b, `","seq":`...)
	b = strconv.AppendInt(b, seq, 10)
	b = append(b, `,"data":`...)
	b = append(b, data...)
	return append(b, '}')
}

func (h *Hub) appendLocked(st *stream, typ string, data []byte) {
	h.pushLocked(st, st.last+1, envelope(typ, st.last+1, data))
}

// pushLocked appends envelope b, whose seq must be st.last+1, and wakes writers.
func (h *Hub) pushLocked(st *stream, seq int64, b []byte) {
	st.last = seq
	now := h.now()
	st.buf = append(st.buf, entry{seq: seq, at: now, b: b})
	st.bytes += len(b)
	st.touched = now
	for len(st.buf) > h.opts.ReplayBuffer || (st.bytes > h.opts.ReplayMaxBytes && len(st.buf) > 1) {
		st.dropFront()
	}
	h.wakeLocked(st)
	h.count("realtime_messages_out_total", "")
}

// wakeLocked notifies st's writers; connections too far behind are closed.
func (h *Hub) wakeLocked(st *stream) {
	first := st.last + 1
	if len(st.buf) > 0 {
		first = st.buf[0].seq
	}
	for _, c := range st.conns {
		live := max(c.cursor, c.liveFrom)
		if st.last-live > int64(h.opts.SendQueue) || c.cursor < first-1 {
			c.close(CodeSlowConsumer, "slow consumer", "slow_consumer")
			continue
		}
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
}

// next returns entries after c's cursor (up to n). lost reports that some were
// evicted before delivery.
func (h *Hub) next(c *conn, n int, dst []entry) (out []entry, lost bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := c.st
	if st == nil || c.cursor >= st.last {
		return dst[:0], false
	}
	if len(st.buf) == 0 || c.cursor < st.buf[0].seq-1 {
		return dst[:0], true
	}
	i := int(c.cursor - st.buf[0].seq + 1)
	j := min(i+n, len(st.buf))
	return append(dst[:0], st.buf[i:j]...), false
}

func (h *Hub) delivered(c *conn, seq int64) {
	h.mu.Lock()
	if c.st != nil && seq > c.cursor {
		c.cursor = seq
	}
	h.mu.Unlock()
}

// ack drops buffered entries up to seq that every attached connection of the
// player has already been sent.
func (h *Hub) ack(c *conn, seq int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := c.st
	if st == nil || seq <= 0 || seq > st.last {
		return false
	}
	limit := seq
	for _, o := range st.conns {
		limit = min(limit, o.cursor)
	}
	for len(st.buf) > 0 && st.buf[0].seq <= limit {
		st.dropFront()
	}
	return true
}

// resume rewinds c to lastSeq when every later message is still buffered.
// Under the bus the buffer is first completed from the shared replay window in
// PostgreSQL, so the answer does not depend on which instance the client hits.
func (h *Hub) resume(ctx context.Context, c *conn, epoch string, lastSeq int64) (ok bool, curEpoch string, cur int64) {
	if h.bus != nil {
		h.mu.Lock()
		st := c.st
		need := st != nil && !st.local && epoch == st.epoch && lastSeq >= 0
		var after int64
		if need {
			after = min(lastSeq, st.last)
		}
		h.mu.Unlock()
		if need {
			h.bus.fill(ctx, key{c.game, c.player}, st, after)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := c.st
	if st == nil {
		return false, "", 0
	}
	ok = epoch == st.epoch && lastSeq >= 0 && lastSeq <= st.last &&
		(lastSeq == st.last || (len(st.buf) > 0 && st.buf[0].seq <= lastSeq+1))
	if ok {
		c.cursor, c.liveFrom = lastSeq, st.last
	}
	return ok, st.epoch, st.last
}

// janitor expires replay entries and idle streams.
func (h *Hub) janitor() {
	defer close(h.jdone)
	iv := min(max(h.opts.ReplayMaxAge/4, 50*time.Millisecond), 30*time.Second)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.sweep()
			if h.bus != nil {
				h.bus.cleanup()
			}
		}
	}
}

func (h *Hub) sweep() {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := h.now().Add(-h.opts.ReplayMaxAge)
	for k, st := range h.streams {
		limit := st.last
		for _, c := range st.conns {
			limit = min(limit, c.cursor)
		}
		for len(st.buf) > 0 && st.buf[0].at.Before(cutoff) && st.buf[0].seq <= limit {
			st.dropFront()
		}
		if len(st.conns) == 0 && len(st.buf) == 0 && !st.syncing && st.touched.Before(cutoff) {
			delete(h.streams, k)
		}
	}
}

// Close shuts the gateway down: new connections are refused, every connection
// gets a 1001 (going away) close frame, and Close waits for them until ctx is
// done, after which remaining connections are dropped.
func (h *Hub) Close(ctx context.Context) error {
	h.mu.Lock()
	first := !h.closed
	h.closed = true
	conns := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	if first {
		close(h.stop)
		<-h.jdone
		if h.bus != nil {
			h.bus.close(ctx) // publishes what is queued, then ends the LISTEN session
		}
	}
	for _, c := range conns {
		c.close(websocket.StatusGoingAway, "server shutting down", "shutdown")
	}
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
		h.cancel()
		return nil
	case <-ctx.Done():
		h.cancel()
		for _, c := range conns {
			_ = c.ws.CloseNow()
		}
		<-done
		return ctx.Err()
	}
}
