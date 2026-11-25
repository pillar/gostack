// Package valkey is a minimal Valkey (Redis-protocol) client: RESP2 over TCP,
// a bounded connection pool, AUTH/SELECT on connect and a hard deadline on
// every command. It implements only what GoStack uses (GET/SET/DEL/PING,
// EVALSHA/EVAL, TIME); there is no pipelining, pub/sub, cluster, Sentinel or
// TLS support.
//
// Connection discipline: a connection carries exactly one command at a time and
// is discarded on any I/O or protocol error (including a deadline), so a
// timed-out reply can never be read by the next command.
package valkey

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Options configures a Client.
type Options struct {
	Addr     string // host:port
	Username string // ACL user (optional; requires Password)
	Password string
	DB       int
	// DialTimeout bounds connect + AUTH/SELECT (default 1s).
	DialTimeout time.Duration
	// Timeout caps every command; a shorter context deadline wins (default 200ms).
	Timeout time.Duration
	// PoolSize bounds open connections; callers beyond it wait until their
	// context deadline or Timeout (default 16).
	PoolSize int
}

// Client is safe for concurrent use.
type Client struct {
	o      Options
	slots  chan struct{} // one token per allowed open connection
	mu     sync.Mutex
	idle   []*conn
	closed bool
}

// ErrClosed is returned after Close.
var ErrClosed = errors.New("valkey: client closed")

// ErrNil is returned by String for a nil bulk reply (missing key).
var ErrNil = errors.New("valkey: nil")

// Error is an error reply sent by the server (e.g. NOSCRIPT, WRONGTYPE).
type Error string

func (e Error) Error() string { return "valkey: " + string(e) }

// New returns a client; it does not connect until the first command.
func New(o Options) *Client {
	if o.DialTimeout <= 0 {
		o.DialTimeout = time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 200 * time.Millisecond
	}
	if o.PoolSize <= 0 {
		o.PoolSize = 16
	}
	return &Client{o: o, slots: make(chan struct{}, o.PoolSize)}
}

// Close closes idle connections; connections in use are closed when released.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, cn := range c.idle {
		cn.nc.Close()
	}
	c.idle = nil
	return nil
}

type conn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
}

// Reply is a decoded RESP2 value.
type Reply struct {
	Str   string // simple string or bulk string
	Int   int64
	Array []Reply
	Nil   bool // nil bulk string or nil array
}

// Do sends one command and returns the reply. A server error reply is returned
// as Error (the connection stays usable); any other error discards the
// connection. The effective deadline is min(ctx deadline, now+Timeout).
func (c *Client) Do(ctx context.Context, args ...string) (Reply, error) {
	ctx, cancel := context.WithTimeout(ctx, c.o.Timeout)
	defer cancel()
	cn, err := c.get(ctx)
	if err != nil {
		return Reply{}, err
	}
	dl, _ := ctx.Deadline()
	_ = cn.nc.SetDeadline(dl)
	// Abort a blocked read/write promptly if ctx is cancelled before the deadline.
	stop := context.AfterFunc(ctx, func() { _ = cn.nc.SetDeadline(time.Unix(1, 0)) })
	rep, err := cn.roundTrip(args)
	stopped := stop()
	var se Error
	if err != nil && !errors.As(err, &se) || !stopped {
		c.discard(cn)
	} else {
		c.put(cn)
	}
	return rep, err
}

// String runs a command whose reply is a bulk string; a nil reply is ErrNil.
func (c *Client) String(ctx context.Context, args ...string) (string, error) {
	r, err := c.Do(ctx, args...)
	if err == nil && r.Nil {
		err = ErrNil
	}
	return r.Str, err
}

// Ping checks connectivity (and credentials, on a fresh connection).
func (c *Client) Ping(ctx context.Context) error {
	r, err := c.Do(ctx, "PING")
	if err == nil && r.Str != "PONG" {
		err = fmt.Errorf("valkey: unexpected PING reply %q", r.Str)
	}
	return err
}

func (c *Client) get(ctx context.Context) (*conn, error) {
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("valkey: pool exhausted: %w", ctx.Err())
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.slots
		return nil, ErrClosed
	}
	if n := len(c.idle); n > 0 {
		cn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return cn, nil
	}
	c.mu.Unlock()
	cn, err := c.dial(ctx)
	if err != nil {
		<-c.slots
		return nil, err
	}
	return cn, nil
}

func (c *Client) put(cn *conn) {
	c.mu.Lock()
	if c.closed {
		cn.nc.Close()
	} else {
		c.idle = append(c.idle, cn)
	}
	c.mu.Unlock()
	<-c.slots
}

func (c *Client) discard(cn *conn) {
	cn.nc.Close()
	<-c.slots
}

func (c *Client) dial(ctx context.Context) (*conn, error) {
	dctx, cancel := context.WithTimeout(ctx, c.o.DialTimeout)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(dctx, "tcp", c.o.Addr)
	if err != nil {
		return nil, fmt.Errorf("valkey: dial: %w", err)
	}
	if dl, ok := dctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	cn := &conn{nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}
	if c.o.Password != "" {
		args := []string{"AUTH", c.o.Password}
		if c.o.Username != "" {
			args = []string{"AUTH", c.o.Username, c.o.Password}
		}
		if _, err := cn.roundTrip(args); err != nil {
			nc.Close()
			return nil, fmt.Errorf("valkey: auth: %w", err)
		}
	}
	if c.o.DB != 0 {
		if _, err := cn.roundTrip([]string{"SELECT", strconv.Itoa(c.o.DB)}); err != nil {
			nc.Close()
			return nil, fmt.Errorf("valkey: select: %w", err)
		}
	}
	return cn, nil
}

func (cn *conn) roundTrip(args []string) (Reply, error) {
	w := cn.w
	w.WriteByte('*')
	w.WriteString(strconv.Itoa(len(args)))
	w.WriteString("\r\n")
	for _, a := range args {
		w.WriteByte('$')
		w.WriteString(strconv.Itoa(len(a)))
		w.WriteString("\r\n")
		w.WriteString(a)
		w.WriteString("\r\n")
	}
	if err := w.Flush(); err != nil {
		return Reply{}, err
	}
	return readReply(cn.r, 0)
}

// maxBulk bounds a single bulk reply (Valkey's proto-max-bulk-len default is 512MB;
// GoStack never stores values this large).
const maxBulk = 64 << 20

var errProtocol = errors.New("valkey: protocol error")

func readReply(r *bufio.Reader, depth int) (Reply, error) {
	if depth > 8 {
		return Reply{}, errProtocol
	}
	line, err := readLine(r)
	if err != nil {
		return Reply{}, err
	}
	if len(line) == 0 {
		return Reply{}, errProtocol
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return Reply{Str: string(body)}, nil
	case '-':
		return Reply{}, Error(body)
	case ':':
		n, err := strconv.ParseInt(string(body), 10, 64)
		if err != nil {
			return Reply{}, errProtocol
		}
		return Reply{Int: n}, nil
	case '$':
		n, err := strconv.Atoi(string(body))
		if err != nil || n < -1 || n > maxBulk {
			return Reply{}, errProtocol
		}
		if n == -1 {
			return Reply{Nil: true}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Reply{}, err
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return Reply{}, errProtocol
		}
		return Reply{Str: string(buf[:n])}, nil
	case '*':
		n, err := strconv.Atoi(string(body))
		if err != nil || n < -1 || n > 1<<16 {
			return Reply{}, errProtocol
		}
		if n == -1 {
			return Reply{Nil: true}, nil
		}
		arr := make([]Reply, n)
		for i := range arr {
			v, err := readReply(r, depth+1)
			var se Error
			if errors.As(err, &se) {
				// An error element inside an array (e.g. EXEC); keep it as text.
				v, err = Reply{Str: string(se)}, nil
			}
			if err != nil {
				return Reply{}, err
			}
			arr[i] = v
		}
		return Reply{Array: arr}, nil
	}
	return Reply{}, errProtocol
}

func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, errProtocol
		}
		return nil, err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, errProtocol
	}
	return line[:len(line)-2], nil
}

// Script is a Lua script run with EVALSHA, falling back to EVAL on NOSCRIPT
// (after a server restart or SCRIPT FLUSH).
type Script struct {
	src, sha string
}

// NewScript computes the script's SHA1.
func NewScript(src string) *Script { return &Script{src: src, sha: sha1hex(src)} }

// Run executes the script with keys and args.
func (s *Script) Run(ctx context.Context, c *Client, keys []string, args ...string) (Reply, error) {
	cmd := make([]string, 0, 3+len(keys)+len(args))
	cmd = append(cmd, "EVALSHA", s.sha, strconv.Itoa(len(keys)))
	cmd = append(append(cmd, keys...), args...)
	r, err := c.Do(ctx, cmd...)
	var se Error
	if errors.As(err, &se) && len(se) >= 8 && se[:8] == "NOSCRIPT" {
		cmd[0], cmd[1] = "EVAL", s.src
		r, err = c.Do(ctx, cmd...)
	}
	return r, err
}

func sha1hex(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
