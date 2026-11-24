package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	c, _ := NewClientIP([]string{"10.0.0.0/8"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	if got := c.Of(r); got != "203.0.113.9" {
		t.Fatalf("untrusted peer header honoured: %s", got)
	}
	r.RemoteAddr = "10.1.1.1:1"
	r.Header.Del("CF-Connecting-IP")
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 1.2.3.4, 10.2.2.2")
	if got := c.Of(r); got != "1.2.3.4" {
		t.Fatalf("xff walk: %s", got)
	}
}
