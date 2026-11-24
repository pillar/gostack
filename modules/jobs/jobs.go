// Package jobs is the durable SQLite task queue (PRD §8).
//
// Semantics: at-least-once. A claim grants a lease and a random lease token;
// Ack/Nack/Extend must present the token, so a worker whose lease expired and
// was re-claimed cannot overwrite the newer execution's result. Handlers must
// make external side effects idempotent. Exactly-once is NOT provided.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/httpx"
)

// ErrLeaseLost means the caller no longer owns the job (lease expired and re-claimed, or finished).
var ErrLeaseLost = errors.New("jobs: lease lost")

const maxPayload = 64 << 10

type Job struct {
	TenantID    string
	Type        string
	Payload     []byte
	RunAt       time.Time // zero = now
	MaxAttempts int       // 0 = 5
	DedupeKey   string    // optional; unique per (tenant, type)
}

type Claimed struct {
	ID       string
	TenantID string
	Type     string
	Payload  []byte
	Attempt  int
	Token    string
}

type Queue struct {
	DB  *sqlite.DB
	Now func() time.Time
}

func (q *Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Enqueue inserts a job inside the caller's transaction, so it commits atomically
// with the business change (transactional outbox). Returns the job id, or the
// existing id when the dedupe key already exists.
func (q *Queue) Enqueue(ctx context.Context, tx *sql.Tx, j Job) (string, error) {
	if j.Type == "" {
		return "", errors.New("jobs: type required")
	}
	if len(j.Payload) > maxPayload {
		return "", fmt.Errorf("jobs: payload %d bytes exceeds %d; store large data in object storage and enqueue a reference", len(j.Payload), maxPayload)
	}
	if j.TenantID == "" {
		j.TenantID = "default"
	}
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = 5
	}
	now := q.now()
	if j.RunAt.IsZero() {
		j.RunAt = now
	}
	if j.Payload == nil {
		j.Payload = []byte{}
	}
	var dedupe any
	if j.DedupeKey != "" {
		dedupe = j.DedupeKey
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE tenant_id=? AND type=? AND dedupe_key=?`, j.TenantID, j.Type, j.DedupeKey).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	id := httpx.RandomID("job_")
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,tenant_id,type,payload,status,run_at,max_attempts,dedupe_key,created_at,updated_at)
		VALUES(?,?,?,?,'pending',?,?,?,?,?)`, id, j.TenantID, j.Type, j.Payload, j.RunAt.UnixMilli(), j.MaxAttempts, dedupe, now.UnixMilli(), now.UnixMilli())
	return id, err
}

// EnqueueNow enqueues in its own transaction.
func (q *Queue) EnqueueNow(ctx context.Context, j Job) (id string, err error) {
	err = q.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		id, err = q.Enqueue(ctx, tx, j)
		return err
	})
	return id, err
}

// Claim atomically leases the next runnable job of the given types (all types when empty).
// Returns nil when none is runnable. Jobs whose lease expired after their final
// attempt are moved to dead first.
func (q *Queue) Claim(ctx context.Context, types []string, lease time.Duration) (*Claimed, error) {
	now := q.now().UnixMilli()
	var c *Claimed
	err := q.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		c = nil
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='dead', lease_token=NULL, updated_at=?,
			last_error=coalesce(last_error,'') || ' [lease expired on final attempt]'
			WHERE status='running' AND lease_until<? AND attempts>=max_attempts`, now, now); err != nil {
			return err
		}
		filter, args := typeFilter(types)
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE
			((status IN ('pending','retry_wait') AND run_at<=?) OR (status='running' AND lease_until<?))`+filter+`
			ORDER BY run_at LIMIT 1`, append([]any{now, now}, args...)...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		cl := &Claimed{ID: id, Token: httpx.RandomID("lt_")}
		err = tx.QueryRowContext(ctx, `UPDATE jobs SET status='running', attempts=attempts+1, lease_until=?, lease_token=?, updated_at=?
			WHERE id=? RETURNING tenant_id, type, payload, attempts`, now+lease.Milliseconds(), cl.Token, now, id).
			Scan(&cl.TenantID, &cl.Type, &cl.Payload, &cl.Attempt)
		if err != nil {
			return err
		}
		c = cl
		return nil
	})
	return c, err
}

func typeFilter(types []string) (string, []any) {
	if len(types) == 0 {
		return "", nil
	}
	s := " AND type IN ("
	args := make([]any, len(types))
	for i, t := range types {
		if i > 0 {
			s += ","
		}
		s += "?"
		args[i] = t
	}
	return s + ")", args
}

// Ack marks the job succeeded if the token still owns it.
func (q *Queue) Ack(ctx context.Context, c *Claimed) error {
	return q.owned(ctx, c, `UPDATE jobs SET status='succeeded', lease_token=NULL, lease_until=NULL, updated_at=? WHERE id=? AND lease_token=? AND status='running'`,
		q.now().UnixMilli(), c.ID, c.Token)
}

// Extend renews the lease for long-running work.
func (q *Queue) Extend(ctx context.Context, c *Claimed, lease time.Duration) error {
	now := q.now().UnixMilli()
	return q.owned(ctx, c, `UPDATE jobs SET lease_until=?, updated_at=? WHERE id=? AND lease_token=? AND status='running'`,
		now+lease.Milliseconds(), now, c.ID, c.Token)
}

// Nack records a failure: retry_wait with exponential backoff and full jitter, or dead after max attempts.
func (q *Queue) Nack(ctx context.Context, c *Claimed, cause error) error {
	now := q.now()
	backoff := min(time.Second<<min(c.Attempt, 16), time.Hour)
	next := now.Add(time.Duration(rand.Int64N(int64(backoff)) + int64(time.Second)))
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	return q.owned(ctx, c, `UPDATE jobs SET
		status=CASE WHEN attempts>=max_attempts THEN 'dead' ELSE 'retry_wait' END,
		run_at=?, last_error=?, lease_token=NULL, lease_until=NULL, updated_at=?
		WHERE id=? AND lease_token=? AND status='running'`, next.UnixMilli(), msg, now.UnixMilli(), c.ID, c.Token)
}

func (q *Queue) owned(ctx context.Context, c *Claimed, query string, args ...any) error {
	return q.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrLeaseLost
		}
		return nil
	})
}

// Replay moves a dead job back to pending with a fresh attempt budget (audited via updated_at/last_error).
func (q *Queue) Replay(ctx context.Context, id string) error {
	now := q.now().UnixMilli()
	return q.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET status='pending', attempts=0, run_at=?, updated_at=?,
			last_error=coalesce(last_error,'') || ' [replayed]' WHERE id=? AND status='dead'`, now, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("jobs: %s is not dead", id)
		}
		return nil
	})
}

// Status returns a job's status (for tests and admin tooling).
func (q *Queue) Status(ctx context.Context, id string) (status string, attempts int, err error) {
	err = q.DB.Read.QueryRowContext(ctx, `SELECT status, attempts FROM jobs WHERE id=?`, id).Scan(&status, &attempts)
	return
}

// Stats reports queue depth and the age of the oldest runnable job.
type Stats struct {
	Runnable     int64
	Dead         int64
	OldestAgeSec float64
}

func (q *Queue) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	var oldest sql.NullInt64
	now := q.now().UnixMilli()
	err := q.DB.Read.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE status IN ('pending','retry_wait','running')),
		count(*) FILTER (WHERE status='dead'),
		min(run_at) FILTER (WHERE status IN ('pending','retry_wait') AND run_at<=?)
		FROM jobs`, now).Scan(&s.Runnable, &s.Dead, &oldest)
	if oldest.Valid {
		s.OldestAgeSec = float64(now-oldest.Int64) / 1000
	}
	return s, err
}

// PurgeSucceeded deletes succeeded jobs older than age.
func (q *Queue) PurgeSucceeded(ctx context.Context, age time.Duration) (int64, error) {
	var n int64
	err := q.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE status='succeeded' AND updated_at<?`, q.now().Add(-age).UnixMilli())
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// Handler processes one job. Returning an error schedules a retry.
type Handler func(ctx context.Context, job *Claimed) error

// Worker polls the queue and runs handlers with a bounded number of goroutines.
type Worker struct {
	Queue    *Queue
	Handlers map[string]Handler
	Workers  int
	Poll     time.Duration
	Lease    time.Duration
	Log      *slog.Logger
}

// Run blocks until ctx is cancelled, then waits for in-flight jobs to finish.
// In-flight handlers get a context that is NOT cancelled by shutdown so they can
// complete and Ack; their lease bounds how long a crash can hide a job.
func (w *Worker) Run(ctx context.Context) {
	types := make([]string, 0, len(w.Handlers))
	for t := range w.Handlers {
		types = append(types, t)
	}
	if len(types) == 0 || w.Workers <= 0 {
		<-ctx.Done()
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < w.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				c, err := w.Queue.Claim(ctx, types, w.Lease)
				if err != nil && ctx.Err() == nil {
					w.Log.Error("job claim failed", "err", err)
				}
				if c == nil {
					select {
					case <-ctx.Done():
					case <-time.After(w.Poll):
					}
					continue
				}
				w.run(context.WithoutCancel(ctx), c)
			}
		}()
	}
	wg.Wait()
}

func (w *Worker) run(ctx context.Context, c *Claimed) {
	hctx, cancel := context.WithTimeout(ctx, w.Lease)
	defer cancel()
	err := func() (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("panic: %v", v)
			}
		}()
		return w.Handlers[c.Type](hctx, c)
	}()
	if err == nil {
		err = w.Queue.Ack(ctx, c)
		if err != nil {
			w.Log.Warn("job ack failed", "job", c.ID, "err", err)
		}
		return
	}
	w.Log.Warn("job failed", "job", c.ID, "type", c.Type, "attempt", c.Attempt, "err", err)
	if nerr := w.Queue.Nack(ctx, c, err); nerr != nil {
		w.Log.Warn("job nack failed", "job", c.ID, "err", nerr)
	}
}
