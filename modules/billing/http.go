package billing

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
)

// Scope is the API key scope required by the billing HTTP API.
const Scope = "billing"

const maxBody = 16 << 10

// Handler registers the /v1/billing/* routes on mux. Every route requires an
// API key (Authorization: Bearer gsk_...) with scope "billing"; the tenant is
// the key's tenant, never a request field. Mutations require an
// Idempotency-Key header.
func (s *Service) Handler(mux *http.ServeMux, keys auth.APIKeys) {
	h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, tenant string) error) {
		mux.Handle(pattern, httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			k, err := keys.Verify(r.Context(), httpx.BearerToken(r))
			if err != nil {
				return err
			}
			if !k.Has(Scope) {
				return httpx.Forbidden("api key lacks billing scope")
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			err = fn(w, r, k.TenantID)
			if errors.Is(err, sqlite.ErrOverloaded) {
				return httpx.Overloaded("write queue full")
			}
			return err
		}))
	}
	// mutate decodes the body into req, runs op with the Idempotency-Key and writes the result.
	mutate := func(pattern string, status int, newReq func() any, op func(ctx context.Context, r *http.Request, tenant, key string, req any) (Result, error)) {
		h(pattern, func(w http.ResponseWriter, r *http.Request, tenant string) error {
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				return httpx.BadRequest("Idempotency-Key header is required")
			}
			req := newReq()
			if err := httpx.DecodeJSON(r, req); err != nil {
				return err
			}
			res, err := op(r.Context(), r, tenant, key, req)
			if err != nil {
				return err
			}
			if res.Replayed {
				w.Header().Set("Idempotent-Replayed", "true")
			}
			httpx.WriteJSON(w, status, res)
			return nil
		})
	}

	type createReq struct {
		ID       string `json:"id"`
		Currency string `json:"currency"`
	}
	type amountReq struct {
		Amount int64  `json:"amount"`
		Note   string `json:"note"`
	}
	type reserveReq struct {
		Amount     int64  `json:"amount"`
		TTLSeconds int64  `json:"ttl_seconds"`
		Note       string `json:"note"`
	}
	type noteReq struct {
		Note string `json:"note"`
	}
	type resolveReq struct {
		Action string `json:"action"`
		Amount int64  `json:"amount"`
		Note   string `json:"note"`
	}

	mutate("POST /v1/billing/accounts", http.StatusCreated, func() any { return &createReq{} },
		func(ctx context.Context, _ *http.Request, t, k string, q any) (Result, error) {
			b := q.(*createReq)
			return s.CreateAccount(ctx, t, k, b.ID, b.Currency)
		})
	mutate("POST /v1/billing/accounts/{id}/credit", http.StatusOK, func() any { return &amountReq{} },
		func(ctx context.Context, r *http.Request, t, k string, q any) (Result, error) {
			b := q.(*amountReq)
			return s.Credit(ctx, t, k, r.PathValue("id"), b.Amount, b.Note)
		})
	mutate("POST /v1/billing/accounts/{id}/debit", http.StatusOK, func() any { return &amountReq{} },
		func(ctx context.Context, r *http.Request, t, k string, q any) (Result, error) {
			b := q.(*amountReq)
			return s.Debit(ctx, t, k, r.PathValue("id"), b.Amount, b.Note)
		})
	mutate("POST /v1/billing/accounts/{id}/reservations", http.StatusCreated, func() any { return &reserveReq{} },
		func(ctx context.Context, r *http.Request, t, k string, q any) (Result, error) {
			b := q.(*reserveReq)
			if b.TTLSeconds < 0 || b.TTLSeconds > int64(MaxReservationTTL/time.Second) {
				return Result{}, httpx.BadRequest("ttl_seconds must be between 1 and 604800")
			}
			return s.Reserve(ctx, t, k, r.PathValue("id"), b.Amount, time.Duration(b.TTLSeconds)*time.Second, b.Note)
		})
	mutate("POST /v1/billing/reservations/{id}/settle", http.StatusOK, func() any { return &amountReq{} },
		func(ctx context.Context, r *http.Request, t, k string, q any) (Result, error) {
			b := q.(*amountReq)
			return s.Settle(ctx, t, k, r.PathValue("id"), b.Amount, b.Note)
		})
	mutate("POST /v1/billing/reservations/{id}/release", http.StatusOK, func() any { return &noteReq{} },
		func(ctx context.Context, r *http.Request, t, k string, q any) (Result, error) {
			return s.Release(ctx, t, k, r.PathValue("id"), q.(*noteReq).Note)
		})
	mutate("POST /v1/billing/reservations/{id}/uncertain", http.StatusOK, func() any { return &noteReq{} },
		func(ctx context.Context, r *http.Request, t, k string, q any) (Result, error) {
			return s.MarkUncertain(ctx, t, k, r.PathValue("id"), q.(*noteReq).Note)
		})
	mutate("POST /v1/billing/reservations/{id}/resolve", http.StatusOK, func() any { return &resolveReq{} },
		func(ctx context.Context, r *http.Request, t, k string, q any) (Result, error) {
			b := q.(*resolveReq)
			return s.ResolveReconciliation(ctx, t, k, r.PathValue("id"), b.Action, b.Amount, b.Note)
		})

	h("GET /v1/billing/accounts/{id}", func(w http.ResponseWriter, r *http.Request, t string) error {
		a, err := s.GetAccount(r.Context(), t, r.PathValue("id"))
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, a)
		return nil
	})
	h("GET /v1/billing/accounts/{id}/entries", func(w http.ResponseWriter, r *http.Request, t string) error {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				return httpx.BadRequest("limit must be 1-200")
			}
			limit = n
		}
		es, err := s.Entries(r.Context(), t, r.PathValue("id"), limit)
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"entries": es})
		return nil
	})
	h("GET /v1/billing/reservations/{id}", func(w http.ResponseWriter, r *http.Request, t string) error {
		res, err := s.GetReservation(r.Context(), t, r.PathValue("id"))
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, res)
		return nil
	})
	h("GET /v1/billing/reconcile", func(w http.ResponseWriter, r *http.Request, t string) error {
		ms, err := s.Reconcile(r.Context(), t)
		if err != nil {
			return err
		}
		if ms == nil {
			ms = []Mismatch{}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": len(ms) == 0, "mismatches": ms})
		return nil
	})
}
