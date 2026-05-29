package sqlitestore

import (
	"context"
	"database/sql"
)

// Querier is the rebinding SQL-access surface used by the store and its sibling
// packages (webhooks, policy, hooks, lfs locks/quota). Every method rebinds the
// query to the backend's placeholder style before delegating. It also carries
// the backend's error classifiers so callers can map constraint errors without
// a separate backend reference.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	RunInTx(ctx context.Context, fn func(tx Tx) error) error
	IsUniqueViolation(err error) bool
	IsCheckViolation(err error) bool
	// Greatest returns the backend's SQL expression for max(expr, floor).
	Greatest(expr, floor string) string
	// SupportsSkipLocked reports whether the backend supports
	// SELECT … FOR UPDATE SKIP LOCKED. true for postgres; false for sqlite/libsql.
	SupportsSkipLocked() bool
}

// Tx is the rebinding transaction surface handed to RunInTx callbacks.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	// InsertReturningID runs an INSERT and returns the generated integer id.
	InsertReturningID(ctx context.Context, query string, args ...any) (int64, error)
	// DeferForeignKeys defers FK checks to COMMIT for this tx.
	DeferForeignKeys() error
}

// TxRunner is implemented by *dbWrap; RunInTx runs fn inside a transaction,
// committing on nil error and rolling back otherwise.
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(tx Tx) error) error
}

// dbWrap wraps *sql.DB + Backend. It is what Store holds and what Store.DB()
// returns.
type dbWrap struct {
	db      *sql.DB
	backend Backend
}

func (w *dbWrap) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return w.db.ExecContext(ctx, w.backend.Rebind(q), args...)
}
func (w *dbWrap) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return w.db.QueryContext(ctx, w.backend.Rebind(q), args...)
}
func (w *dbWrap) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return w.db.QueryRowContext(ctx, w.backend.Rebind(q), args...)
}
func (w *dbWrap) IsUniqueViolation(err error) bool { return w.backend.IsUniqueViolation(err) }
func (w *dbWrap) IsCheckViolation(err error) bool  { return w.backend.IsCheckViolation(err) }
func (w *dbWrap) Greatest(expr, floor string) string {
	return w.backend.Greatest(expr, floor)
}
func (w *dbWrap) SupportsSkipLocked() bool { return w.backend.SupportsSkipLocked() }

func (w *dbWrap) Close() error { return w.db.Close() }
func (w *dbWrap) raw() *sql.DB { return w.db }

// BeginTx begins a transaction and returns a rebinding *txWrap.
func (w *dbWrap) BeginTx(ctx context.Context, opts *sql.TxOptions) (*txWrap, error) {
	tx, err := w.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &txWrap{tx: tx, backend: w.backend}, nil
}

// RunInTx runs fn inside a transaction, committing on nil error and rolling
// back on an error return or a panic (the panic is re-raised after rollback).
func (w *dbWrap) RunInTx(ctx context.Context, fn func(tx Tx) error) (err error) {
	tx, err := w.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// txWrap wraps *sql.Tx + Backend with the same rebinding surface.
type txWrap struct {
	tx      *sql.Tx
	backend Backend
}

func (w *txWrap) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return w.tx.ExecContext(ctx, w.backend.Rebind(q), args...)
}
func (w *txWrap) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return w.tx.QueryContext(ctx, w.backend.Rebind(q), args...)
}
func (w *txWrap) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return w.tx.QueryRowContext(ctx, w.backend.Rebind(q), args...)
}
func (w *txWrap) IsUniqueViolation(err error) bool { return w.backend.IsUniqueViolation(err) }
func (w *txWrap) IsCheckViolation(err error) bool  { return w.backend.IsCheckViolation(err) }
func (w *txWrap) Greatest(expr, floor string) string {
	return w.backend.Greatest(expr, floor)
}
func (w *txWrap) SupportsSkipLocked() bool { return w.backend.SupportsSkipLocked() }
func (w *txWrap) DeferForeignKeys() error  { return w.backend.DeferForeignKeys(w.tx) }
func (w *txWrap) InsertReturningID(ctx context.Context, query string, args ...any) (int64, error) {
	return w.backend.InsertReturningID(ctx, w.tx, query, args...)
}
func (w *txWrap) Commit() error   { return w.tx.Commit() }
func (w *txWrap) Rollback() error { return w.tx.Rollback() }
func (w *txWrap) raw() *sql.Tx    { return w.tx }

// NewTestQuerier wraps a raw *sql.DB as a sqlite-backed Querier for tests in
// sibling packages. Test-only convenience.
func NewTestQuerier(db *sql.DB) Querier { return &dbWrap{db: db, backend: sqliteBackend{}} }
