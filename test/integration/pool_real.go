//go:build integration

package integration

// pool_real.go — testPool backed by database/sql + lib/pq.
//
// Uses lib/pq (pure-Go, no golang.org/x/* deps) so this file compiles in
// environments where proxy.golang.org and golang.org are unreachable.
//
// On the real production machine with pgx in go.sum, db.NewSQLPool can be
// swapped for pgxpool.New — *pgxpool.Pool satisfies db.Pool directly.
//
// Usage:
//   DATABASE_URL=postgres://... go test -tags=integration ./test/integration/...

import (
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
)

// testPool opens a real PostgreSQL connection and registers t.Cleanup to close it.
// Skips the test if DATABASE_URL is not set.
func testPool(t *testing.T) db.Pool {
	t.Helper()
	dbURL := db.DatabaseURL() // panics with clear message if DATABASE_URL unset
	pool, err := db.NewSQLPool(dbURL)
	if err != nil {
		t.Fatalf("testPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
