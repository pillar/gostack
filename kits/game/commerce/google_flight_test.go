package commerce_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/kits/game/commerce"
)

// slowServer answers each request after a value is sent on (or close of)
// release; started receives one value per request.
type slowServer struct {
	srv     *httptest.Server
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	n       int
}

func newSlowServer(t *testing.T, body func() string) *slowServer {
	s := &slowServer{started: make(chan struct{}, 100), release: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.n++
		s.mu.Unlock()
		s.started <- struct{}{}
		<-s.release
		w.Write([]byte(body()))
	}))
	t.Cleanup(func() {
		s.releaseAll()
		s.srv.Close()
	})
	return s
}

func (s *slowServer) releaseAll() { s.once.Do(func() { close(s.release) }) }

func (s *slowServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// A slow token endpoint must not serialize every caller behind a mutex held
// during network IO: one refresh is in flight and other callers wait for it
// only as long as their own context allows.
func TestGoogleAccessTokenSingleFlight(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	s := newSlowServer(t, func() string { return `{"access_token":"at-slow","expires_in":3600}` })
	sa := &commerce.GoogleServiceAccount{Email: "svc@example.com", Key: key, TokenURL: s.srv.URL}
	results := make(chan string, 5)
	for range 5 {
		go func() {
			tok, _ := sa.AccessToken(context.Background())
			results <- tok
		}()
	}
	<-s.started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, err := sa.AccessToken(ctx); err == nil {
		t.Fatal("expected the short deadline to expire")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("short-deadline caller blocked %v behind the in-flight refresh", d)
	}
	s.releaseAll()
	for range 5 {
		if tok := <-results; tok != "at-slow" {
			t.Fatalf("token %q", tok)
		}
	}
	if n := s.requests(); n != 1 {
		t.Fatalf("token requests=%d, want 1 (single flight)", n)
	}
}

func TestJWKSFetcherSingleFlight(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := func() string {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "n": n, "e": e}}})
		return string(b)
	}
	s := newSlowServer(t, jwks)
	f := &commerce.JWKSFetcher{URL: s.srv.URL, TTL: 50 * time.Millisecond, MinRefresh: time.Millisecond}
	// Cold cache: a short-deadline caller gives up without waiting for the slow fetch.
	done := make(chan error, 4)
	for range 4 {
		go func() { _, err := f.RSAKey(context.Background(), "k1"); done <- err }()
	}
	<-s.started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, err := f.RSAKey(ctx, "k1"); err == nil {
		t.Fatal("expected the short deadline to expire")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("short-deadline caller blocked %v", d)
	}
	s.release <- struct{}{} // answer the one in-flight fetch
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := s.requests(); n != 1 {
		t.Fatalf("jwks requests=%d, want 1", n)
	}
	// Stale cache: callers get the cached key at once while one refresh runs.
	time.Sleep(60 * time.Millisecond)
	for range 3 {
		t0 := time.Now()
		k, err := f.RSAKey(context.Background(), "k1")
		if err != nil || k == nil || time.Since(t0) > time.Second {
			t.Fatalf("stale key not served promptly: %v %v", err, time.Since(t0))
		}
	}
	<-s.started
	s.releaseAll()
	if n := s.requests(); n != 2 {
		t.Fatalf("jwks requests=%d, want 2", n)
	}
}
