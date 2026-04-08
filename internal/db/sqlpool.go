package db

// sqlpool.go — database/sql adapter implementing db.Pool.
//
// Used by:
//   - services (resource-manager, host-agent CLI) in main.go via NewSQLPool()
//   - integration tests via pool_real.go
//
// Build tag: //go:build integration
// This file is excluded from standard unit test builds to avoid requiring
// the lib/pq dependency when running handler tests on a macOS dev box
// without network access to the module proxy.
//
// Driver: lib/pq registers as "postgres" and uses $N placeholder syntax,
// which matches all SQL in this codebase (PostgreSQL $1, $2, ... style).

import (
	"context"
	"database/sql"
	"fmt"

)

// NewSQLPool opens a *sql.DB using the lib/pq driver and wraps it as a db.Pool.
// Call once at service startup. Safe for concurrent use.
//
// databaseURL format: "postgres://user:pass@host:5432/dbname?sslmode=disable"
func NewSQLPool(databaseURL string) (Pool, error) {
	sqlDB, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	// Verify connectivity immediately.
	if err := sqlDB.PingContext(context.Background()); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}
	return &sqlPool{db: sqlDB}, nil
}

// sqlPool wraps *sql.DB to implement db.Pool.
type sqlPool struct {
	db *sql.DB
}

func (p *sqlPool) Exec(ctx context.Context, query string, args ...any) (CommandTag, error) {
	result, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlCommandTag{result: result}, nil
}

func (p *sqlPool) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRows{rows: rows}, nil
}

// sqlRows wraps *sql.Rows to implement db.Rows.
type sqlRows struct {
	rows *sql.Rows
}

func (r *sqlRows) Next() bool             { return r.rows.Next() }
func (r *sqlRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *sqlRows) Close()                 { r.rows.Close() }
func (r *sqlRows) Err() error             { return r.rows.Err() }

func (p *sqlPool) QueryRow(ctx context.Context, query string, args ...any) Row {
	return p.db.QueryRowContext(ctx, query, args...)
}

func (p *sqlPool) Close() {
	p.db.Close()
}

// sqlCommandTag wraps sql.Result to implement db.CommandTag.
type sqlCommandTag struct {
	result sql.Result
}

func (t *sqlCommandTag) RowsAffected() int64 {
	n, _ := t.result.RowsAffected()
	return n
}
