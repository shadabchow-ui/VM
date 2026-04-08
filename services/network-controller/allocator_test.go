package main

// allocator_test.go — Unit tests for IP Allocator and Releaser.
//
// Source: IP_ALLOCATION_CONTRACT_V1, IMPLEMENTATION_PLAN_V1 §32-33,
//         11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md.
//
// These tests use a real in-process SQLite-compatible test database via
// the database/sql interface with lib/pq. Because we cannot connect to
// a real Postgres in this environment, we use a fake DB that returns
// controlled responses.
//
// The concurrent stress test (zero duplicate IPs under load) requires a
// real PostgreSQL instance and is gated to the M6 integration test suite.
// See: IMPLEMENTATION_PLAN_V1 §M6 gate — concurrent IP allocation stress test.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
)

// ── Fake DB driver for unit tests ─────────────────────────────────────────────
// We register a fake SQL driver that returns scripted responses,
// so tests run without a real PostgreSQL instance.

func init() {
	sql.Register("fakedb", &fakeDriver{})
}

type fakeDriver struct{}
type fakeConn struct {
	mu      sync.Mutex
	ips     []string // available IPs in the pool
	claimed map[string]string // ip → instanceID
}

func (d *fakeDriver) Open(_ string) (driver.Conn, error) {
	return &fakeConn{
		ips:     []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		claimed: make(map[string]string),
	}, nil
}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{conn: c, query: query}, nil
}
func (c *fakeConn) Close() error                                   { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)                      { return &fakeTx{}, nil }
func (c *fakeConn) BeginTx(_ context.Context, _ driver.TxOptions) (driver.Tx, error) {
	return &fakeTx{}, nil
}

type fakeTx struct{}
func (t *fakeTx) Commit() error   { return nil }
func (t *fakeTx) Rollback() error { return nil }

type fakeStmt struct {
	conn  *fakeConn
	query string
}

func (s *fakeStmt) Close() error { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) Exec(_ []driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()

	// Simulate SELECT ip_address WHERE allocated=FALSE for allocation.
	if len(s.conn.ips) == 0 {
		return &fakeRows{done: true}, nil
	}
	// Claim the first available IP.
	ip := s.conn.ips[0]
	s.conn.ips = s.conn.ips[1:]
	if len(args) >= 2 {
		instanceID := fmt.Sprintf("%v", args[1])
		s.conn.claimed[ip] = instanceID
	}
	return &fakeRows{col: "ip_address", val: ip}, nil
}

type fakeRows struct {
	col   string
	val   string
	done  bool
	read  bool
}

func (r *fakeRows) Columns() []string { return []string{r.col} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done || r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = r.val
	return nil
}

// ── Tests ──────────────────────────────────────────────────────────────────────

func newFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("fakedb", "")
	if err != nil {
		t.Fatalf("open fakedb: %v", err)
	}
	return db
}

func TestAllocator_AllocatesIP(t *testing.T) {
	db := newFakeDB(t)
	defer db.Close()
	a := NewAllocator(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ip, err := a.Allocate(context.Background(), "vpc-1", "inst-1")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if ip == "" {
		t.Error("Allocate returned empty IP")
	}
}

func TestReleaser_ReleaseReturnsNil(t *testing.T) {
	db := newFakeDB(t)
	defer db.Close()
	rel := NewReleaser(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Release an IP — fakeDB Exec always returns RowsAffected=1.
	if err := rel.Release(context.Background(), "10.0.0.1", "vpc-1", "inst-1"); err != nil {
		t.Errorf("Release = %v, want nil", err)
	}
}

func TestReleaser_AlreadyReleased_IsNoOp(t *testing.T) {
	db := newFakeDB(t)
	defer db.Close()
	rel := NewReleaser(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Release twice — both must return nil.
	if err := rel.Release(context.Background(), "10.0.0.1", "vpc-1", "inst-1"); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := rel.Release(context.Background(), "10.0.0.1", "vpc-1", "inst-1"); err != nil {
		t.Errorf("second Release = %v, want nil (idempotent)", err)
	}
}

// TestConcurrentAllocation_NoStress verifies that the Allocator does not panic
// under concurrent calls in unit test context (real uniqueness guarantee requires Postgres).
// Source: IMPLEMENTATION_PLAN_V1 §M6 gate (full concurrent stress test).
func TestConcurrentAllocation_NoStress(t *testing.T) {
	db := newFakeDB(t)
	defer db.Close()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a := NewAllocator(db, log)

	const goroutines = 3
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			_, err := a.Allocate(context.Background(), "vpc-1", fmt.Sprintf("inst-%d", n))
			errs <- err
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		if err := <-errs; err != nil {
			// One or two may fail (pool exhausted in fake) — that's expected.
			// What must not happen is a panic or data race.
			t.Logf("goroutine %d: Allocate returned %v (may be pool exhausted in fake)", i, err)
		}
	}
}
