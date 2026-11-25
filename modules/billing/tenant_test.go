package billing_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pillar/gostack/core/auth"
)

// A billing key without a tenant must not reach any route: the library treats
// an empty tenant as "all tenants" in ReconcileIn (operator CLI use only).
func TestHTTPRejectsKeyWithoutTenant(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	keys := auth.APIKeys{DB: s.DB}
	_, k1, err := keys.Create(ctx, "t1", "svc", []string{"billing"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, kEmpty, err := keys.Create(ctx, "", "svc", []string{"billing"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Handler(mux, keys)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(method, path, key, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Idempotency-Key", "k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, b := do("POST", "/v1/billing/accounts", k1, `{"id":"u1","currency":"GEM"}`); st != 201 {
		t.Fatalf("%d %s", st, b)
	}
	for _, r := range [][3]string{
		{"GET", "/v1/billing/reconcile", ""},
		{"GET", "/v1/billing/accounts/u1", ""},
		{"POST", "/v1/billing/accounts", `{"id":"u2","currency":"GEM"}`},
		{"POST", "/v1/billing/accounts/u1/credit", `{"amount":1}`},
	} {
		if st, b := do(r[0], r[1], kEmpty, r[2]); st != 403 || strings.Contains(b, "u1") {
			t.Fatalf("%s %s: %d %s", r[0], r[1], st, b)
		}
	}
}
