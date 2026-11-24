// Package httpx provides the JSON error envelope, request IDs and the standard
// middleware chain (recover, request ID, body limit, timeout, concurrency cap).
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error is an API error with a stable code.
type Error struct {
	Status     int            `json:"-"`
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
	RetryAfter time.Duration  `json:"-"`
	Cause      error          `json:"-"`
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Message + ": " + e.Cause.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func BadRequest(msg string) *Error     { return NewError(http.StatusBadRequest, "invalid_request", msg) }
func Unauthorized(msg string) *Error   { return NewError(http.StatusUnauthorized, "unauthorized", msg) }
func Forbidden(msg string) *Error      { return NewError(http.StatusForbidden, "forbidden", msg) }
func NotFound(msg string) *Error       { return NewError(http.StatusNotFound, "not_found", msg) }
func Conflict(code, msg string) *Error { return NewError(http.StatusConflict, code, msg) }
func TooLarge(msg string) *Error {
	return NewError(http.StatusRequestEntityTooLarge, "payload_too_large", msg)
}
func NotImplemented(msg string) *Error {
	return NewError(http.StatusNotImplemented, "not_implemented", msg)
}
func RateLimited(retry time.Duration) *Error {
	e := NewError(http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
	e.RetryAfter = retry
	return e
}
func Overloaded(msg string) *Error {
	e := NewError(http.StatusServiceUnavailable, "overloaded", msg)
	e.RetryAfter = time.Second
	return e
}

type ctxKey int

const requestIDKey ctxKey = 0

// RequestID returns the request ID stored in ctx.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

// RandomID returns prefix + 26 base32 chars (128 bits of entropy).
func RandomID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// WriteJSON writes v with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError renders err as the standard envelope. Non-API errors become 500
// and are logged with the request ID; their text is never sent to the client.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			e = TooLarge("request body too large")
		case errors.Is(err, context.DeadlineExceeded):
			e = NewError(http.StatusServiceUnavailable, "overloaded", "request deadline exceeded")
		default:
			slog.ErrorContext(r.Context(), "internal error", "err", err, "request_id", RequestID(r.Context()), "path", r.URL.Path)
			e = NewError(http.StatusInternalServerError, "internal", "internal error")
		}
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((e.RetryAfter+time.Second-1)/time.Second)))
	}
	WriteJSON(w, e.Status, map[string]any{"error": e, "request_id": RequestID(r.Context())})
}

// HandlerFunc is an http handler that returns an error rendered by WriteError.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

func (h HandlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		WriteError(w, r, err)
	}
}

// DecodeJSON strictly decodes a JSON body (unknown fields rejected, single value).
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return TooLarge("request body too large")
		}
		if errors.Is(err, io.EOF) {
			return BadRequest("empty body")
		}
		return BadRequest(fmt.Sprintf("invalid JSON: %v", err))
	}
	if dec.More() {
		return BadRequest("trailing data after JSON value")
	}
	return nil
}

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so the first one is outermost.
func Chain(h http.Handler, ms ...Middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// WithRequestID assigns a server-generated request ID (client values are not trusted).
func WithRequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := RandomID("req_")
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
		})
	}
}

// WithRecover converts panics into 500 responses.
func WithRecover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					WriteError(w, r, fmt.Errorf("panic: %v", v))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// WithLimits applies a body size cap, a per-request deadline and a global
// in-flight cap; excess requests get 503 immediately instead of queueing.
func WithLimits(maxBody int64, timeout time.Duration, maxConcurrent int) Middleware {
	sem := make(chan struct{}, maxConcurrent)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			default:
				WriteError(w, r, Overloaded("server busy"))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// StatusRecorder captures the response status for logging/metrics.
type StatusRecorder struct {
	http.ResponseWriter
	Status int
}

func (s *StatusRecorder) WriteHeader(code int) {
	if s.Status == 0 {
		s.Status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *StatusRecorder) Write(b []byte) (int, error) {
	if s.Status == 0 {
		s.Status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *StatusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ClientIP resolves the caller address. Forwarding headers are honoured only
// when the direct peer is inside a trusted proxy CIDR.
type ClientIP struct{ trusted []*net.IPNet }

func NewClientIP(cidrs []string) (*ClientIP, error) {
	c := &ClientIP{}
	for _, s := range cidrs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, err
		}
		c.trusted = append(c.trusted, n)
	}
	return c, nil
}

func (c *ClientIP) Of(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !c.isTrusted(ip) {
		return host
	}
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); net.ParseIP(v) != nil {
		return v
	}
	// Walk X-Forwarded-For right to left, skipping trusted hops.
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := net.ParseIP(strings.TrimSpace(parts[i]))
		if p == nil {
			break
		}
		if !c.isTrusted(p) {
			return p.String()
		}
	}
	return host
}

func (c *ClientIP) isTrusted(ip net.IP) bool {
	for _, n := range c.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// BearerToken extracts the token from "Authorization: Bearer <t>".
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}
