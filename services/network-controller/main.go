package main

// main.go — Network Controller service entrypoint.
//
// Source: IMPLEMENTATION_PLAN_V1 §32-34 (IP allocation, release, NAT programming).
//
// Exposes two internal HTTP endpoints (mTLS-authenticated via the internal gateway):
//   POST /internal/v1/ip/allocate  — atomic IP allocation (SELECT FOR UPDATE SKIP LOCKED)
//   POST /internal/v1/ip/release   — idempotent IP release
//
// The worker's INSTANCE_CREATE handler calls allocate before issuing CreateInstance
// to the Host Agent. The worker's INSTANCE_DELETE rollback handler calls release.
//
// Environment:
//   DATABASE_URL  PostgreSQL connection string (required)
//   LISTEN_ADDR   HTTP listen address (default :8083)

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	_ "github.com/lib/pq"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Error("DATABASE_URL not set")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open db", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Error("db ping failed", "error", err)
		os.Exit(1)
	}

	allocator := NewAllocator(db, log)
	releaser := NewReleaser(db, log)

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8083"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/ip/allocate", allocator.handleAllocate)
	mux.HandleFunc("/internal/v1/ip/release", releaser.handleRelease)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	log.Info("network-controller starting", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil { //nolint:gosec
		log.Error("server exited", "error", err)
		os.Exit(1)
	}
}

// ── HTTP helpers shared by allocator.go and releaser.go ──────────────────────

func decodeJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
