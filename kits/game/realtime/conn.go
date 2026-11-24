package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/pillar/gostack/core/httpx"
)

// clientMsg is any client→server message.
type clientMsg struct {
	Type    string          `json:"type"`
	Token   string          `json:"token,omitempty"`
	Seq     int64           `json:"seq,omitempty"`
	LastSeq *int64          `json:"last_seq,omitempty"`
	Epoch   string          `json:"epoch,omitempty"`
	ID      string          `json:"id,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type resumeReq struct {
	epoch string
	last  int64
}

// ctrl is a server control message (no seq) or a resume request for the writer.
type ctrl struct {
	b      []byte
	resume *resumeReq
}

// conn is one WebSocket connection. Goroutines: run (auth + read loop) and
// writer; the ping timer runs only while a ping is in flight.
type conn struct {
	h       *Hub
	ws      *websocket.Conn
	game    string
	player  string
	tokHash string

	// guarded by h.mu
	st       *stream
	cursor   int64 // last seq handed to the socket
	liveFrom int64 // seq at attach/resume; earlier messages are replay, not queue

	notify chan struct{}
	ctrl   chan ctrl
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	closeDone chan struct{}

	pingMu      sync.Mutex
	pingTimer   *time.Timer
	pingStopped bool
	lastCheck   time.Time

	tokens float64 // inbound bucket (read loop only)
	last   time.Time
}

func newConn(h *Hub, ws *websocket.Conn, gameID string) *conn {
	c := &conn{h: h, ws: ws, game: gameID, notify: make(chan struct{}, 1), ctrl: make(chan ctrl, 8),
		closeDone: make(chan struct{}), tokens: float64(h.opts.InboundBurst), last: time.Now()}
	c.ctx, c.cancel = context.WithCancel(h.base)
	ws.SetReadLimit(h.opts.MaxMessageBytes)
	return c
}

// close starts a close handshake with code (once); reason labels the metric.
func (c *conn) close(code websocket.StatusCode, msg, reason string) {
	c.closeOnce.Do(func() {
		c.h.count("realtime_closed_total", reason)
		go func() {
			_ = c.ws.Close(code, msg)
			c.cancel()
			close(c.closeDone)
		}()
	})
}

// abort drops the connection without a handshake (dead peer, client gone).
func (c *conn) abort(reason string) {
	c.closeOnce.Do(func() {
		c.h.count("realtime_closed_total", reason)
		_ = c.ws.CloseNow()
		c.cancel()
		close(c.closeDone)
	})
}

func (c *conn) run(player, tokHash string) {
	h := c.h
	writerDone := make(chan struct{})
	started := false
	defer func() {
		c.abort("client")
		<-c.closeDone
		c.cancel()
		c.pingMu.Lock()
		c.pingStopped = true
		if c.pingTimer != nil {
			c.pingTimer.Stop()
		}
		c.pingMu.Unlock()
		if started {
			<-writerDone
		}
		h.detach(c)
	}()
	if !h.register(c) {
		c.close(websocket.StatusGoingAway, "server shutting down", "shutdown")
		return
	}
	if player == "" {
		var ok bool
		if player, tokHash, ok = c.readAuth(); !ok {
			return
		}
	}
	c.player, c.tokHash = player, tokHash
	epoch, last, ok := h.attach(c)
	if !ok {
		c.close(websocket.StatusGoingAway, "server shutting down", "shutdown")
		return
	}
	welcome, _ := json.Marshal(map[string]any{"type": "welcome", "player_id": player, "epoch": epoch, "seq": last})
	h.count("realtime_connections_total", "")
	started = true
	go c.writer(welcome, writerDone)
	c.lastCheck = h.now()
	c.pingMu.Lock()
	c.pingTimer = time.AfterFunc(h.opts.PingInterval, c.ping)
	c.pingMu.Unlock()
	c.readLoop()
}

// readAuth waits for {"type":"auth","token":"..."} within AuthTimeout.
func (c *conn) readAuth() (player, tokHash string, ok bool) {
	h := c.h
	t := time.AfterFunc(h.opts.AuthTimeout, func() { c.close(CodeUnauthorized, "auth timeout", "auth_timeout") })
	defer t.Stop()
	typ, data, err := c.ws.Read(c.ctx)
	if err != nil {
		c.readErr(err)
		return "", "", false
	}
	var m clientMsg
	if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != "auth" || m.Token == "" {
		c.close(CodeUnauthorized, "auth message required", "auth")
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(c.ctx, h.opts.MessageTimeout)
	defer cancel()
	player, tokHash, err = h.authenticate(ctx, c.game, m.Token)
	var he *httpx.Error
	switch {
	case err == nil:
		return player, tokHash, true
	case errors.As(err, &he) && he.Status == 403:
		c.close(CodeForbidden, he.Message, "auth")
	case errors.As(err, &he) && he.Status == 401:
		c.close(CodeUnauthorized, he.Message, "auth")
	default:
		h.log.Error("realtime auth failed", "err", err)
		c.close(websocket.StatusInternalError, "internal error", "internal")
	}
	return "", "", false
}

func (c *conn) readErr(err error) {
	switch {
	case errors.Is(err, websocket.ErrMessageTooBig):
		c.abort("too_large") // the library already sent 1009
	default:
		c.abort("client")
	}
}

func (c *conn) allow() bool {
	now := time.Now()
	o := c.h.opts
	c.tokens = min(float64(o.InboundBurst), c.tokens+now.Sub(c.last).Seconds()*o.InboundRate)
	c.last = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

func (c *conn) readLoop() {
	h := c.h
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.readErr(err)
			return
		}
		if !c.allow() {
			c.close(CodeRateLimited, "rate limited", "rate_limited")
			return
		}
		h.count("realtime_messages_in_total", "")
		var m clientMsg
		if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type == "" {
			c.sendError("", "invalid_message", "expected a JSON text message with a type")
			continue
		}
		switch m.Type {
		case "ping":
			c.sendCtrl(map[string]any{"type": "pong"})
		case "ack":
			if !h.ack(c, m.Seq) {
				c.sendError(m.ID, "invalid_ack", "seq out of range")
			}
		case "resume":
			if m.LastSeq == nil {
				c.sendError(m.ID, "invalid_message", "last_seq required")
				continue
			}
			c.push(ctrl{resume: &resumeReq{epoch: m.Epoch, last: *m.LastSeq}})
		case "auth":
			c.sendError(m.ID, "already_authenticated", "connection is already authenticated")
		default:
			c.dispatch(m)
		}
	}
}

func (c *conn) dispatch(m clientMsg) {
	h := c.h
	if h.onMessage == nil || !validType(m.Type) {
		c.sendError(m.ID, "unknown_type", "unsupported message type")
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, h.opts.MessageTimeout)
	defer cancel()
	err := h.onMessage(ctx, c.game, c.player, Message{Type: m.Type, ID: m.ID, Data: m.Data})
	var he *httpx.Error
	switch {
	case err == nil:
		if m.ID != "" {
			c.sendCtrl(map[string]any{"type": "ok", "ref": m.ID})
		}
	case errors.As(err, &he):
		c.sendError(m.ID, he.Code, he.Message)
	default:
		h.log.Error("realtime handler failed", "type", m.Type, "err", err)
		c.sendError(m.ID, "internal", "internal error")
	}
}

func (c *conn) sendError(ref, code, msg string) {
	m := map[string]any{"type": "error", "code": code, "message": msg}
	if ref != "" {
		m["ref"] = ref
	}
	c.sendCtrl(m)
}

func (c *conn) sendCtrl(v any) {
	b, _ := json.Marshal(v)
	c.push(ctrl{b: b})
}

// push queues a control item; a client that does not drain its control replies
// is a slow consumer.
func (c *conn) push(x ctrl) {
	select {
	case c.ctrl <- x:
	default:
		c.close(CodeSlowConsumer, "slow consumer", "slow_consumer")
	}
}

func (c *conn) writer(welcome []byte, done chan struct{}) {
	defer close(done)
	if !c.write(welcome) {
		return
	}
	var batch []entry
	flush := func() bool {
		for {
			var lost bool
			batch, lost = c.h.next(c, 16, batch)
			if lost {
				c.close(CodeSlowConsumer, "slow consumer", "slow_consumer")
				return false
			}
			if len(batch) == 0 {
				return true
			}
			for _, e := range batch {
				if !c.write(e.b) {
					return false
				}
				c.h.delivered(c, e.seq)
			}
		}
	}
	for {
		select {
		case <-c.ctx.Done():
			return
		case x := <-c.ctrl:
			if x.resume == nil {
				if !c.write(x.b) {
					return
				}
				continue
			}
			ok, epoch, cur := c.h.resume(c, x.resume.epoch, x.resume.last)
			var b []byte
			if ok {
				c.h.count("realtime_resume_total", "ok")
				b, _ = json.Marshal(map[string]any{"type": "resumed", "last_seq": x.resume.last, "epoch": epoch, "seq": cur})
			} else {
				c.h.count("realtime_resume_total", "failed")
				b, _ = json.Marshal(map[string]any{"type": "resume_failed", "epoch": epoch, "seq": cur})
			}
			if !c.write(b) || !flush() {
				return
			}
		case <-c.notify:
			if !flush() {
				return
			}
		}
	}
}

func (c *conn) write(b []byte) bool {
	ctx, cancel := context.WithTimeout(c.ctx, c.h.opts.WriteTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		if c.ctx.Err() == nil {
			if errors.Is(err, context.DeadlineExceeded) {
				c.close(CodeSlowConsumer, "slow consumer", "slow_consumer")
			} else {
				c.abort("write_error")
			}
		}
		return false
	}
	return true
}

// ping sends a WebSocket ping and drops the connection when no pong arrives in
// time; it also re-validates the session every SessionRecheck.
func (c *conn) ping() {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	if c.pingStopped || c.ctx.Err() != nil {
		return
	}
	h := c.h
	ctx, cancel := context.WithTimeout(c.ctx, h.opts.PongTimeout)
	err := c.ws.Ping(ctx)
	cancel()
	if err != nil {
		if c.ctx.Err() == nil {
			c.abort("ping_timeout")
		}
		return
	}
	if now := h.now(); now.Sub(c.lastCheck) >= h.opts.SessionRecheck {
		ctx, cancel := context.WithTimeout(c.ctx, h.opts.MessageTimeout)
		_, err := h.checkSession(ctx, c.game, c.tokHash)
		cancel()
		var he *httpx.Error
		if errors.As(err, &he) {
			c.close(CodeUnauthorized, "session revoked or expired", "session_revoked")
			return
		}
		if err == nil {
			c.lastCheck = now
		}
	}
	c.pingTimer.Reset(h.opts.PingInterval)
}
