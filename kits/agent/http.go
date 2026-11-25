package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/idempotency"
)

// Scope is the API key scope required by the agent HTTP API.
const Scope = "agent"

const maxBody = maxInputBytes + 4<<10

// Routes registers /v1/agent/*. Every route requires an API key with scope
// "agent"; the tenant is the key's tenant (never a request field).
func (k *Kit) Routes(mux *http.ServeMux, keys auth.APIKeys) {
	h := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, tenant string) error) {
		mux.Handle(pattern, httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			key, err := keys.Verify(r.Context(), httpx.BearerToken(r))
			if err != nil {
				return err
			}
			if !key.Has(Scope) {
				return httpx.Forbidden("api key lacks agent scope")
			}
			if strings.TrimSpace(key.TenantID) == "" {
				return httpx.Forbidden("api key has no tenant")
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			err = fn(w, r, key.TenantID)
			if errors.Is(err, store.ErrOverloaded) {
				return httpx.Overloaded("write queue full")
			}
			return err
		}))
	}
	h("POST /v1/agent/runs", k.httpCreate)
	h("GET /v1/agent/runs", func(w http.ResponseWriter, r *http.Request, tenant string) error {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		runs, err := k.List(r.Context(), tenant, q.Get("status"), q.Get("before"), limit)
		if err != nil {
			return err
		}
		next := ""
		if len(runs) > 0 && (limit <= 0 || len(runs) == min(limit, 100)) {
			next = runs[len(runs)-1].ID
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs, "next_before": next})
		return nil
	})
	h("GET /v1/agent/runs/{id}", func(w http.ResponseWriter, r *http.Request, tenant string) error {
		run, err := k.Get(r.Context(), tenant, r.PathValue("id"))
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, run)
		return nil
	})
	h("POST /v1/agent/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request, tenant string) error {
		run, err := k.Cancel(r.Context(), tenant, r.PathValue("id"))
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, run)
		return nil
	})
	h("GET /v1/agent/runs/{id}/audit", func(w http.ResponseWriter, r *http.Request, tenant string) error {
		calls, err := k.Audit(r.Context(), tenant, r.PathValue("id"))
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"tool_calls": calls})
		return nil
	})
	h("POST /v1/agent/runs/{id}/steps/{seq}/resolve", func(w http.ResponseWriter, r *http.Request, tenant string) error {
		seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
		if err != nil || seq < 0 {
			return httpx.BadRequest("seq must be a non-negative integer")
		}
		var req struct {
			Outcome     string          `json:"outcome"`
			Output      json.RawMessage `json:"output"`
			ExternalRef string          `json:"external_ref"`
			Note        string          `json:"note"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			return err
		}
		run, err := k.Resolve(r.Context(), tenant, r.PathValue("id"), seq, req.Outcome, []byte(req.Output), req.ExternalRef, req.Note)
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, run)
		return nil
	})
}

// httpCreate starts a run. An optional Idempotency-Key makes retries return
// the first response instead of starting a second run.
func (k *Kit) httpCreate(w http.ResponseWriter, r *http.Request, tenant string) error {
	var req CreateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	body, _ := json.Marshal(req)
	status, resp, replayed, err := idempotency.Do(r.Context(), k.DB, "agent:"+tenant, r.Header.Get("Idempotency-Key"), "create_run", body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			info, err := k.CreateTx(r.Context(), tx, tenant, req)
			if err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Status: http.StatusCreated, Body: info}, nil
		})
	if err != nil {
		return err
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}
