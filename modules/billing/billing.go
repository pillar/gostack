// Package billing is the optional prepaid-balance ledger (PRD §7): accounts with
// integer minor-unit balances, an append-only ledger, and reservations that are
// persisted before a caller performs an external call and then settled,
// released, expired or sent to manual reconciliation.
//
// Every mutation runs in one store.DB.WriteTx and is keyed by a per-tenant
// idempotency key: the same key with the same request returns the stored result
// (Result.Replayed), the same key with a different request is rejected with
// 409 idempotency_key_reused. Failed operations are not recorded, so a key whose
// first attempt failed (e.g. insufficient_funds) may be retried.
//
// This is not a payment or clearing system: it never talks to a payment
// provider and performs no network IO.
package billing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/httpx"
)

// MaxAmount bounds single amounts and balances (available+reserved) to the
// largest integer a JavaScript client can represent exactly.
const MaxAmount int64 = 1<<53 - 1

// Reservation TTL bounds.
const (
	DefaultReservationTTL = 15 * time.Minute
	MaxReservationTTL     = 7 * 24 * time.Hour
)

const maxNote = 512

// Reservation statuses.
const (
	StatusReserved    = "reserved"
	StatusSettled     = "settled"
	StatusReleased    = "released"
	StatusExpired     = "expired"
	StatusReconciling = "reconciling"
)

// Service implements the billing operations. Now defaults to time.Now.
type Service struct {
	DB  *store.DB
	Now func() time.Time
}

type Account struct {
	TenantID  string `json:"tenant_id"`
	ID        string `json:"id"`
	Currency  string `json:"currency"`
	Available int64  `json:"available"`
	Reserved  int64  `json:"reserved"`
	Version   int64  `json:"version"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type Entry struct {
	ID             string `json:"id"`
	AccountID      string `json:"account_id"`
	Type           string `json:"type"`
	Amount         int64  `json:"amount"`
	DeltaAvailable int64  `json:"delta_available"`
	DeltaReserved  int64  `json:"delta_reserved"`
	ReservationID  string `json:"reservation_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	Note           string `json:"note,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

type Reservation struct {
	ID            string `json:"id"`
	AccountID     string `json:"account_id"`
	Amount        int64  `json:"amount"`
	SettledAmount *int64 `json:"settled_amount,omitempty"`
	Status        string `json:"status"`
	ExpiresAt     int64  `json:"expires_at"`
	Note          string `json:"note,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// Result is the outcome of a mutation. Entry and Reservation are set when the
// operation produced or changed them.
type Result struct {
	Account     Account      `json:"account"`
	Entry       *Entry       `json:"entry,omitempty"`
	Reservation *Reservation `json:"reservation,omitempty"`
	// Replayed is true when the result was returned from the idempotency store.
	Replayed bool `json:"-"`
}

// Mismatch is a reconciliation finding.
type Mismatch struct {
	TenantID  string `json:"tenant_id"`
	AccountID string `json:"account_id"`
	// Field is "available" or "reserved" (stored balance vs ledger sum), or
	// "open_reservations" (stored reserved vs sum of reserved+reconciling reservations).
	Field    string `json:"field"`
	Stored   int64  `json:"stored"`
	Expected int64  `json:"expected"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Errors. All are *httpx.Error with stable codes.
func errInsufficient() error {
	return httpx.Conflict("insufficient_funds", "available balance is insufficient")
}
func errState(r Reservation, want string) error {
	return httpx.Conflict("invalid_state", fmt.Sprintf("reservation is %s, want %s", r.Status, want))
}

// ValidID reports whether id is 1–64 chars of [A-Za-z0-9_.:-].
func ValidID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

func validCurrency(c string) bool {
	if len(c) == 0 || len(c) > 16 || c[0] < 'A' || c[0] > 'Z' {
		return false
	}
	for i := 0; i < len(c); i++ {
		if !(c[i] >= 'A' && c[i] <= 'Z' || c[i] >= '0' && c[i] <= '9' || c[i] == '_') {
			return false
		}
	}
	return true
}

// validKey: 1–128 printable ASCII; the "sys:" prefix is reserved for entries the
// service writes itself (expiry).
func validKey(k string) bool {
	if len(k) == 0 || len(k) > 128 || strings.HasPrefix(k, "sys:") {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

func checkAmount(a int64, allowZero bool) error {
	lo := int64(1)
	if allowZero {
		lo = 0
	}
	if a < lo || a > MaxAmount {
		return httpx.BadRequest(fmt.Sprintf("amount must be an integer in [%d, %d]", lo, MaxAmount))
	}
	return nil
}

func checkNote(n string) error {
	if len(n) > maxNote {
		return httpx.BadRequest("note too long")
	}
	return nil
}

// exec runs fn under per-tenant idempotency in one write transaction.
func (s *Service) exec(ctx context.Context, tenant, key, op string, req any, fn func(tx *sql.Tx, now int64) (Result, error)) (Result, error) {
	if tenant == "" {
		return Result{}, httpx.BadRequest("tenant required")
	}
	if !validKey(key) {
		return Result{}, httpx.BadRequest("idempotency key must be 1-128 printable ASCII characters and must not start with sys:")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	h := sha256.New()
	h.Write([]byte(op))
	h.Write([]byte{0})
	h.Write(body)
	hash := hex.EncodeToString(h.Sum(nil))

	var res Result
	err = s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res = Result{}
		var gotHash string
		var stored []byte
		err := tx.QueryRowContext(ctx, `SELECT request_hash, response FROM billing_idempotency WHERE tenant_id=? AND key=?`, tenant, key).Scan(&gotHash, &stored)
		switch {
		case err == nil:
			if gotHash != hash {
				return httpx.Conflict("idempotency_key_reused", "idempotency key was used with a different request")
			}
			if err := json.Unmarshal(stored, &res); err != nil {
				return err
			}
			res.Replayed = true
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		now := s.now().UnixMilli()
		r, err := fn(tx, now)
		if err != nil {
			return err
		}
		out, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_idempotency(tenant_id,key,op,request_hash,response,created_at) VALUES(?,?,?,?,?,?)`,
			tenant, key, op, hash, out, now); err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

const accountCols = `tenant_id,id,currency,available,reserved,version,created_at,updated_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := row.Scan(&a.TenantID, &a.ID, &a.Currency, &a.Available, &a.Reserved, &a.Version, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func getAccount(ctx context.Context, q querier, tenant, id string) (Account, error) {
	a, err := scanAccount(q.QueryRowContext(ctx, `SELECT `+accountCols+` FROM billing_accounts WHERE tenant_id=? AND id=?`, tenant, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, httpx.NotFound("account not found")
	}
	return a, err
}

const reservationCols = `id,account_id,amount,settled_amount,status,expires_at,note,created_at,updated_at`

func getReservation(ctx context.Context, q querier, tenant, id string) (Reservation, error) {
	var r Reservation
	err := q.QueryRowContext(ctx, `SELECT `+reservationCols+` FROM billing_reservations WHERE tenant_id=? AND id=?`, tenant, id).
		Scan(&r.ID, &r.AccountID, &r.Amount, &r.SettledAmount, &r.Status, &r.ExpiresAt, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, httpx.NotFound("reservation not found")
	}
	return r, err
}

// applyBalance changes an account's balances with a conditional update that
// keeps available >= 0 and available+reserved <= MaxAmount, then appends the
// matching ledger entry.
func applyBalance(ctx context.Context, tx *sql.Tx, tenant string, e *Entry, now int64) (Account, error) {
	res, err := tx.ExecContext(ctx, `UPDATE billing_accounts
		SET available=available+?1, reserved=reserved+?2, version=version+1, updated_at=?3
		WHERE tenant_id=?4 AND id=?5 AND available+?1 >= 0 AND reserved+?2 >= 0 AND available+reserved+?1+?2 <= ?6`,
		e.DeltaAvailable, e.DeltaReserved, now, tenant, e.AccountID, MaxAmount)
	if err != nil {
		return Account{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a, err := getAccount(ctx, tx, tenant, e.AccountID)
		if err != nil {
			return Account{}, err
		}
		if a.Available+e.DeltaAvailable < 0 {
			return Account{}, errInsufficient()
		}
		if a.Reserved+e.DeltaReserved < 0 {
			return Account{}, fmt.Errorf("billing: reserved balance of %s/%s would become negative", tenant, e.AccountID)
		}
		return Account{}, httpx.Conflict("balance_limit", "balance would exceed the maximum amount")
	}
	e.ID = httpx.RandomID("le_")
	e.CreatedAt = now
	var resID any
	if e.ReservationID != "" {
		resID = e.ReservationID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_ledger_entries(id,tenant_id,account_id,type,amount,delta_available,delta_reserved,reservation_id,idempotency_key,note,created_at,seq)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,(SELECT version FROM billing_accounts WHERE tenant_id=? AND id=?))`,
		e.ID, tenant, e.AccountID, e.Type, e.Amount, e.DeltaAvailable, e.DeltaReserved, resID, e.IdempotencyKey, e.Note, now, tenant, e.AccountID); err != nil {
		return Account{}, err
	}
	return getAccount(ctx, tx, tenant, e.AccountID)
}

// CreateAccount creates an account with zero balances. id may be empty to
// generate one.
func (s *Service) CreateAccount(ctx context.Context, tenant, key, id, currency string) (Result, error) {
	if id != "" && !ValidID(id) {
		return Result{}, httpx.BadRequest("account id must be 1-64 chars of [A-Za-z0-9_.:-]")
	}
	if !validCurrency(currency) {
		return Result{}, httpx.BadRequest("currency must be 1-16 chars of [A-Z0-9_] starting with a letter")
	}
	req := struct{ ID, Currency string }{id, currency}
	return s.exec(ctx, tenant, key, "create_account", req, func(tx *sql.Tx, now int64) (Result, error) {
		aid := id
		if aid == "" {
			aid = httpx.RandomID("acct_")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO billing_accounts(tenant_id,id,currency,created_at,updated_at) VALUES(?,?,?,?,?)`, tenant, aid, currency, now, now)
		if store.IsUniqueViolation(err) {
			return Result{}, httpx.Conflict("account_exists", "account already exists")
		}
		if err != nil {
			return Result{}, err
		}
		a, err := getAccount(ctx, tx, tenant, aid)
		return Result{Account: a}, err
	})
}

func (s *Service) simple(ctx context.Context, tenant, key, op, accountID string, amount int64, note string, entry func() Entry) (Result, error) {
	if err := checkAmount(amount, false); err != nil {
		return Result{}, err
	}
	if err := checkNote(note); err != nil {
		return Result{}, err
	}
	req := struct {
		Account string
		Amount  int64
		Note    string
	}{accountID, amount, note}
	return s.exec(ctx, tenant, key, op, req, func(tx *sql.Tx, now int64) (Result, error) {
		e := entry()
		e.AccountID, e.Amount, e.IdempotencyKey, e.Note = accountID, amount, key, note
		a, err := applyBalance(ctx, tx, tenant, &e, now)
		if err != nil {
			return Result{}, err
		}
		return Result{Account: a, Entry: &e}, nil
	})
}

// Credit adds amount to the available balance (top-up).
func (s *Service) Credit(ctx context.Context, tenant, key, accountID string, amount int64, note string) (Result, error) {
	return s.simple(ctx, tenant, key, "credit", accountID, amount, note, func() Entry {
		return Entry{Type: "credit", DeltaAvailable: amount}
	})
}

// Debit charges amount directly from the available balance; it fails with
// insufficient_funds rather than overdrawing.
func (s *Service) Debit(ctx context.Context, tenant, key, accountID string, amount int64, note string) (Result, error) {
	return s.simple(ctx, tenant, key, "debit", accountID, amount, note, func() Entry {
		return Entry{Type: "debit", DeltaAvailable: -amount}
	})
}

// Reserve moves amount from available to reserved and persists a reservation
// that expires after ttl (0 = DefaultReservationTTL). The reservation is
// committed when Reserve returns, before the caller performs its external call.
func (s *Service) Reserve(ctx context.Context, tenant, key, accountID string, amount int64, ttl time.Duration, note string) (Result, error) {
	if err := checkAmount(amount, false); err != nil {
		return Result{}, err
	}
	if err := checkNote(note); err != nil {
		return Result{}, err
	}
	if ttl == 0 {
		ttl = DefaultReservationTTL
	}
	if ttl < time.Second || ttl > MaxReservationTTL {
		return Result{}, httpx.BadRequest("reservation ttl must be between 1s and 7d")
	}
	req := struct {
		Account string
		Amount  int64
		TTL     int64
		Note    string
	}{accountID, amount, ttl.Milliseconds(), note}
	return s.exec(ctx, tenant, key, "reserve", req, func(tx *sql.Tx, now int64) (Result, error) {
		if _, err := getAccount(ctx, tx, tenant, accountID); err != nil {
			return Result{}, err
		}
		r := Reservation{ID: httpx.RandomID("rsv_"), AccountID: accountID, Amount: amount, Status: StatusReserved,
			ExpiresAt: now + ttl.Milliseconds(), Note: note, CreatedAt: now, UpdatedAt: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_reservations(tenant_id,id,account_id,amount,status,expires_at,note,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, tenant, r.ID, r.AccountID, r.Amount, r.Status, r.ExpiresAt, r.Note, now, now); err != nil {
			return Result{}, err
		}
		e := Entry{AccountID: accountID, Type: "reserve", Amount: amount, DeltaAvailable: -amount, DeltaReserved: amount,
			ReservationID: r.ID, IdempotencyKey: key, Note: note}
		a, err := applyBalance(ctx, tx, tenant, &e, now)
		if err != nil {
			return Result{}, err
		}
		return Result{Account: a, Entry: &e, Reservation: &r}, nil
	})
}

// finish moves a reservation from `from` to a terminal status, charging
// `charged` and returning the remainder to available.
func finish(ctx context.Context, tx *sql.Tx, tenant string, r Reservation, from, to, entryType string, charged int64, key, note string, now int64) (Result, error) {
	var settled any
	if to == StatusSettled {
		settled = charged
	}
	res, err := tx.ExecContext(ctx, `UPDATE billing_reservations SET status=?, settled_amount=?, updated_at=? WHERE tenant_id=? AND id=? AND status=?`,
		to, settled, now, tenant, r.ID, from)
	if err != nil {
		return Result{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Result{}, errState(r, from)
	}
	amount := r.Amount - charged // released portion
	if entryType == "settle" {
		amount = charged
	}
	e := Entry{AccountID: r.AccountID, Type: entryType, Amount: amount, DeltaAvailable: r.Amount - charged, DeltaReserved: -r.Amount,
		ReservationID: r.ID, IdempotencyKey: key, Note: note}
	a, err := applyBalance(ctx, tx, tenant, &e, now)
	if err != nil {
		return Result{}, err
	}
	r, err = getReservation(ctx, tx, tenant, r.ID)
	if err != nil {
		return Result{}, err
	}
	return Result{Account: a, Entry: &e, Reservation: &r}, nil
}

func (s *Service) transition(ctx context.Context, tenant, key, op, reservationID string, req any, fn func(tx *sql.Tx, r Reservation, now int64) (Result, error)) (Result, error) {
	return s.exec(ctx, tenant, key, op, req, func(tx *sql.Tx, now int64) (Result, error) {
		r, err := getReservation(ctx, tx, tenant, reservationID)
		if err != nil {
			return Result{}, err
		}
		return fn(tx, r, now)
	})
}

type resReq struct {
	Reservation string
	Action      string `json:",omitempty"`
	Amount      int64
	Note        string
}

// Settle charges actual (0 ≤ actual ≤ reserved amount) and releases the
// remainder. Only reservations in status reserved that have not passed
// expires_at can be settled.
func (s *Service) Settle(ctx context.Context, tenant, key, reservationID string, actual int64, note string) (Result, error) {
	if err := checkAmount(actual, true); err != nil {
		return Result{}, err
	}
	if err := checkNote(note); err != nil {
		return Result{}, err
	}
	return s.transition(ctx, tenant, key, "settle", reservationID, resReq{Reservation: reservationID, Amount: actual, Note: note},
		func(tx *sql.Tx, r Reservation, now int64) (Result, error) {
			if r.Status != StatusReserved {
				return Result{}, errState(r, StatusReserved)
			}
			if now >= r.ExpiresAt {
				return Result{}, httpx.Conflict("reservation_expired", "reservation has expired; mark it uncertain or release it")
			}
			if actual > r.Amount {
				return Result{}, httpx.BadRequest("settle amount exceeds reserved amount")
			}
			return finish(ctx, tx, tenant, r, StatusReserved, StatusSettled, "settle", actual, key, note, now)
		})
}

// Release returns the whole reserved amount to available.
func (s *Service) Release(ctx context.Context, tenant, key, reservationID, note string) (Result, error) {
	if err := checkNote(note); err != nil {
		return Result{}, err
	}
	return s.transition(ctx, tenant, key, "release", reservationID, resReq{Reservation: reservationID, Note: note},
		func(tx *sql.Tx, r Reservation, now int64) (Result, error) {
			if r.Status != StatusReserved {
				return Result{}, errState(r, StatusReserved)
			}
			return finish(ctx, tx, tenant, r, StatusReserved, StatusReleased, "release", 0, key, note, now)
		})
}

// MarkUncertain moves a reserved reservation to reconciling: the outcome of the
// external call is unknown. The funds stay reserved, the sweeper will not
// expire it and nothing is retried or charged until ResolveReconciliation.
func (s *Service) MarkUncertain(ctx context.Context, tenant, key, reservationID, note string) (Result, error) {
	if err := checkNote(note); err != nil {
		return Result{}, err
	}
	return s.transition(ctx, tenant, key, "mark_uncertain", reservationID, resReq{Reservation: reservationID, Note: note},
		func(tx *sql.Tx, r Reservation, now int64) (Result, error) {
			if r.Status != StatusReserved {
				return Result{}, errState(r, StatusReserved)
			}
			res, err := tx.ExecContext(ctx, `UPDATE billing_reservations SET status='reconciling', note=?, updated_at=? WHERE tenant_id=? AND id=? AND status='reserved'`,
				note, now, tenant, r.ID)
			if err != nil {
				return Result{}, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return Result{}, errState(r, StatusReserved)
			}
			a, err := getAccount(ctx, tx, tenant, r.AccountID)
			if err != nil {
				return Result{}, err
			}
			r, err = getReservation(ctx, tx, tenant, r.ID)
			return Result{Account: a, Reservation: &r}, err
		})
}

// ResolveReconciliation is the operator decision for a reconciling
// reservation: action "settle" charges actual (≤ amount) and releases the rest,
// "release" returns everything. note (the operator's audit note) is required.
func (s *Service) ResolveReconciliation(ctx context.Context, tenant, key, reservationID, action string, actual int64, note string) (Result, error) {
	if note == "" {
		return Result{}, httpx.BadRequest("an operator note is required")
	}
	if err := checkNote(note); err != nil {
		return Result{}, err
	}
	switch action {
	case "settle":
		if err := checkAmount(actual, true); err != nil {
			return Result{}, err
		}
	case "release":
		if actual != 0 {
			return Result{}, httpx.BadRequest("amount must be 0 for release")
		}
	default:
		return Result{}, httpx.BadRequest(`action must be "settle" or "release"`)
	}
	return s.transition(ctx, tenant, key, "resolve", reservationID, resReq{Reservation: reservationID, Action: action, Amount: actual, Note: note},
		func(tx *sql.Tx, r Reservation, now int64) (Result, error) {
			if r.Status != StatusReconciling {
				return Result{}, errState(r, StatusReconciling)
			}
			if action == "release" {
				return finish(ctx, tx, tenant, r, StatusReconciling, StatusReleased, "release", 0, key, note, now)
			}
			if actual > r.Amount {
				return Result{}, httpx.BadRequest("settle amount exceeds reserved amount")
			}
			return finish(ctx, tx, tenant, r, StatusReconciling, StatusSettled, "settle", actual, key, note, now)
		})
}

// ExpireReservations releases up to limit reservations (all tenants) that are
// still reserved past expires_at, in one transaction. Reconciling
// reservations are never expired. It returns the number expired.
func (s *Service) ExpireReservations(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}
	var n int
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		n = 0
		now := s.now().UnixMilli()
		rows, err := tx.QueryContext(ctx, `SELECT tenant_id, `+reservationCols+` FROM billing_reservations
			WHERE status='reserved' AND expires_at<=? ORDER BY expires_at LIMIT ?`, now, limit)
		if err != nil {
			return err
		}
		type item struct {
			tenant string
			r      Reservation
		}
		var due []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.tenant, &it.r.ID, &it.r.AccountID, &it.r.Amount, &it.r.SettledAmount, &it.r.Status, &it.r.ExpiresAt, &it.r.Note, &it.r.CreatedAt, &it.r.UpdatedAt); err != nil {
				rows.Close()
				return err
			}
			due = append(due, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, it := range due {
			if _, err := finish(ctx, tx, it.tenant, it.r, StatusReserved, StatusExpired, "expire", 0, "sys:expire:"+it.r.ID, "", now); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// Maintain runs the expiry sweeper; call it from a periodic maintenance loop.
func (s *Service) Maintain(ctx context.Context) error {
	for {
		n, err := s.ExpireReservations(ctx, 500)
		if err != nil || n < 500 {
			return err
		}
	}
}

// Reconcile compares stored balances with ledger sums and with open
// reservations, for one tenant or (tenant == "") all tenants. It reads a
// consistent snapshot from the read pool and scans the ledger; schedule it
// off-peak. An empty result means the books balance.
func (s *Service) Reconcile(ctx context.Context, tenant string) ([]Mismatch, error) {
	var out []Mismatch
	err := s.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = ReconcileIn(ctx, tx, tenant)
		return err
	})
	return out, err
}

// ReconcileIn is Reconcile over an existing transaction (e.g. the copy
// transaction of `migrate to-postgres`). It returns nil when the books balance.
// An empty tenant checks every tenant: that is for operator tooling only (the
// CLI); the HTTP API refuses keys without a tenant, so it is never reachable
// from a request.
func ReconcileIn(ctx context.Context, tx *sql.Tx, tenant string) ([]Mismatch, error) {
	var out []Mismatch
	err := func() error {
		rows, err := tx.QueryContext(ctx, `SELECT a.tenant_id, a.id, a.available, a.reserved,
			COALESCE((SELECT SUM(delta_available) FROM billing_ledger_entries l WHERE l.tenant_id=a.tenant_id AND l.account_id=a.id),0),
			COALESCE((SELECT SUM(delta_reserved) FROM billing_ledger_entries l WHERE l.tenant_id=a.tenant_id AND l.account_id=a.id),0),
			COALESCE((SELECT SUM(amount) FROM billing_reservations r WHERE r.tenant_id=a.tenant_id AND r.account_id=a.id AND r.status IN ('reserved','reconciling')),0)
			FROM billing_accounts a WHERE ?1='' OR a.tenant_id=?1 ORDER BY a.tenant_id, a.id`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t, id string
			var avail, resv, lAvail, lResv, open int64
			if err := rows.Scan(&t, &id, &avail, &resv, &lAvail, &lResv, &open); err != nil {
				return err
			}
			if avail != lAvail {
				out = append(out, Mismatch{t, id, "available", avail, lAvail})
			}
			if resv != lResv {
				out = append(out, Mismatch{t, id, "reserved", resv, lResv})
			}
			if resv != open {
				out = append(out, Mismatch{t, id, "open_reservations", resv, open})
			}
		}
		return rows.Err()
	}()
	return out, err
}

// GetAccount reads an account.
func (s *Service) GetAccount(ctx context.Context, tenant, id string) (Account, error) {
	return getAccount(ctx, s.DB.Read, tenant, id)
}

// GetReservation reads a reservation.
func (s *Service) GetReservation(ctx context.Context, tenant, id string) (Reservation, error) {
	return getReservation(ctx, s.DB.Read, tenant, id)
}

// Entries lists an account's most recent ledger entries, newest first.
func (s *Service) Entries(ctx context.Context, tenant, accountID string, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if _, err := s.GetAccount(ctx, tenant, accountID); err != nil {
		return nil, err
	}
	rows, err := s.DB.Read.QueryContext(ctx, `SELECT id,account_id,type,amount,delta_available,delta_reserved,COALESCE(reservation_id,''),idempotency_key,note,created_at
		FROM billing_ledger_entries WHERE tenant_id=? AND account_id=? ORDER BY seq DESC LIMIT ?`, tenant, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.AccountID, &e.Type, &e.Amount, &e.DeltaAvailable, &e.DeltaReserved, &e.ReservationID, &e.IdempotencyKey, &e.Note, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
