// Package idempotency stores the first successful response for (scope, key) in
// the same transaction as the business change, so a retried request either
// replays that response or, if the original never committed, executes once.
package idempotency

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/httpx"
)

// DefaultTTL covers client retry windows (SDK offline retry is bounded well below this).
const DefaultTTL = 72 * time.Hour

// ValidKey reports whether k is 1–128 printable ASCII characters.
func ValidKey(k string) bool {
	if len(k) == 0 || len(k) > 128 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// RequestHash binds the key to the operation and the request payload.
func RequestHash(op string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(op))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Result is a JSON response. Body is encoded with json.Marshal (which compacts
// and HTML-escapes embedded json.RawMessage); when Raw is set it is used
// verbatim instead, for responses whose bytes are part of the contract (e.g. a
// payload covered by a checksum). Raw must be valid JSON.
type Result struct {
	Status int
	Body   any
	Raw    []byte
}

// Do runs fn in a write transaction. When key is empty there is no idempotency
// protection. When a record for (scope,key) exists with the same request hash its
// stored response is returned (replayed=true) without calling fn; a different
// hash yields 409 idempotency_key_reused. Errors from fn roll everything back and
// nothing is recorded, so the client may retry with the same key.
func Do(ctx context.Context, db *store.DB, scope, key, op string, body []byte, fn func(tx *sql.Tx) (Result, error)) (status int, resp []byte, replayed bool, err error) {
	if key != "" && !ValidKey(key) {
		return 0, nil, false, httpx.BadRequest("Idempotency-Key must be 1-128 printable ASCII characters")
	}
	hash := RequestHash(op, body)
	// Keys are unique per (scope, operation), matching the API contract.
	scope = scope + "|" + op
	err = db.WriteTx(ctx, func(tx *sql.Tx) error {
		replayed, status, resp = false, 0, nil
		if key != "" {
			var gotHash string
			err := tx.QueryRowContext(ctx, `SELECT request_hash, response_code, response_body FROM idempotency_records
				WHERE scope=? AND key=? AND expires_at>?`, scope, key, time.Now().UnixMilli()).Scan(&gotHash, &status, &resp)
			switch {
			case err == nil:
				if gotHash != hash {
					return httpx.Conflict("idempotency_key_reused", "Idempotency-Key was used with a different request")
				}
				replayed = true
				return nil
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		r, err := fn(tx)
		if err != nil {
			return err
		}
		if r.Status == 0 {
			r.Status = http.StatusOK
		}
		b := r.Raw
		if b == nil {
			if b, err = json.Marshal(r.Body); err != nil {
				return err
			}
		}
		status, resp = r.Status, b
		if key == "" {
			return nil
		}
		now := time.Now()
		// The upsert also overwrites an expired record with the same key.
		_, err = tx.ExecContext(ctx, `INSERT INTO idempotency_records(scope,key,op,request_hash,response_code,response_body,created_at,expires_at)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(scope,key) DO UPDATE SET op=excluded.op, request_hash=excluded.request_hash,
			response_code=excluded.response_code, response_body=excluded.response_body, created_at=excluded.created_at, expires_at=excluded.expires_at`, scope, key, op, hash, status, resp, now.UnixMilli(), now.Add(DefaultTTL).UnixMilli())
		return err
	})
	return status, resp, replayed, err
}

// Write renders a Do result, marking replays.
func Write(w http.ResponseWriter, status int, body []byte, replayed bool) {
	w.Header().Set("Content-Type", "application/json")
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// PurgeExpired deletes expired records.
func PurgeExpired(ctx context.Context, db *store.DB) (int64, error) {
	var n int64
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM idempotency_records WHERE expires_at<=?`, time.Now().UnixMilli())
		if err == nil {
			n, _ = res.RowsAffected()
		}
		return err
	})
	return n, err
}
