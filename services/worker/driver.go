//go:build integration

package db

// driver.go — Registers the lib/pq PostgreSQL driver.
//
// Tagged //go:build integration so unit test builds (go test ./services/worker/handlers/...)
// do not require lib/pq to be available in the module cache.
// Integration test builds (go test -tags=integration ./test/integration/...) compile
// this file and register the driver via its init() side effect.
// Production service binaries are built with -tags=integration or link lib/pq directly.

import _ "github.com/lib/pq" // registers "postgres" driver
