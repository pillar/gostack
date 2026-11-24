package observability

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccessLogRecordsRoutePatternAndRedacts(t *testing.T) {
	reg := NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {})
	h := AccessLog(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))(CaptureRoute(mux))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/items/42", nil))
	var buf bytes.Buffer
	reg.Render(&buf)
	if !strings.Contains(buf.String(), `http_requests_total{route="GET /items/{id}",class="2xx"} 1`) {
		t.Fatalf("metrics:\n%s", buf.String())
	}
	var out bytes.Buffer
	NewLogger(&out, "info", "json").Info("x", "access_token", "abc", "guest_secret", "s")
	if strings.Contains(out.String(), "abc") || strings.Contains(out.String(), `"s"`) {
		t.Fatalf("not redacted: %s", out.String())
	}
}
