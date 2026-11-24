package postgres

import (
	"context"
	"database/sql/driver"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// connector wraps the pgx stdlib connector so every connection rewrites '?'
// placeholders (Rebind), stores Go bools as 0/1 integers (the portable schema
// has no BOOLEAN columns, matching SQLite) and remembers retryable failures for
// store.DB.WriteTx.
type connector struct{ base driver.Connector }

func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &conn{base: dc}, nil
}

func (c connector) Driver() driver.Driver { return c.base.Driver() }

// baseConn is the method set of *stdlib.Conn that conn forwards.
type baseConn interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
}

type conn struct {
	base      driver.Conn
	retryable bool
}

// IsRetryable reports SQLSTATE 40001 (serialization_failure) or 40P01
// (deadlock_detected): the transaction can be re-run from the start.
func IsRetryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}

func (c *conn) note(err error) error {
	if err != nil && IsRetryable(err) {
		c.retryable = true
	}
	return err
}

// TakeRetryable reports (and clears) whether a statement on this connection
// failed with a retryable error since the last call.
func (c *conn) TakeRetryable() bool {
	r := c.retryable
	c.retryable = false
	return r
}

func (c *conn) b() baseConn { return c.base.(baseConn) }

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	s, err := c.b().PrepareContext(ctx, Rebind(query))
	if err != nil {
		return nil, c.note(err)
	}
	return &stmt{base: s, c: c}, nil
}

func (c *conn) Close() error { return c.base.Close() }

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	t, err := c.b().BeginTx(ctx, opts)
	if err != nil {
		return nil, c.note(err)
	}
	return &tx{base: t, c: c}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.b().ExecContext(ctx, Rebind(query), args)
	return r, c.note(err)
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	r, err := c.b().QueryContext(ctx, Rebind(query), args)
	return r, c.note(err)
}

func (c *conn) Ping(ctx context.Context) error { return c.b().Ping(ctx) }

func (c *conn) ResetSession(ctx context.Context) error {
	c.retryable = false
	return c.b().ResetSession(ctx)
}

// CheckNamedValue converts bool to 0/1 and otherwise lets pgx encode the value.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if b, ok := nv.Value.(bool); ok {
		if b {
			nv.Value = int64(1)
		} else {
			nv.Value = int64(0)
		}
	}
	return nil
}

type stmt struct {
	base driver.Stmt
	c    *conn
}

func (s *stmt) Close() error  { return s.base.Close() }
func (s *stmt) NumInput() int { return s.base.NumInput() }
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errors.New("postgres: Stmt.Exec without context is not supported")
}
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("postgres: Stmt.Query without context is not supported")
}
func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	r, err := s.base.(driver.StmtExecContext).ExecContext(ctx, args)
	return r, s.c.note(err)
}
func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	r, err := s.base.(driver.StmtQueryContext).QueryContext(ctx, args)
	return r, s.c.note(err)
}

type tx struct {
	base driver.Tx
	c    *conn
}

func (t *tx) Commit() error   { return t.c.note(t.base.Commit()) }
func (t *tx) Rollback() error { return t.base.Rollback() }
