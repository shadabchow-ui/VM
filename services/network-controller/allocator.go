package main

// allocator.go — Atomic IP allocation using SELECT FOR UPDATE SKIP LOCKED.
//
// Source: IP_ALLOCATION_CONTRACT_V1, IMPLEMENTATION_PLAN_V1 §32.
//
// Invariant I-2: no two instances share the same IP within a VPC.
// Enforced by:
//   1. UNIQUE(vpc_id, ip_address) constraint in schema.
//   2. SELECT FOR UPDATE SKIP LOCKED in the allocation transaction.
//
// Transaction pattern (exactly as specified in IP_ALLOCATION_CONTRACT_V1):
//   BEGIN
//   SELECT ip_address FROM ip_allocations
//     WHERE vpc_id=$1 AND allocated=FALSE
//     ORDER BY ip_address LIMIT 1 FOR UPDATE SKIP LOCKED
//   UPDATE ip_allocations SET allocated=TRUE, owner_instance_id=$2
//     WHERE ip_address=$3 AND vpc_id=$1
//   COMMIT
//
// The network controller HTTP service wraps this in a POST /internal/v1/ip/allocate
// endpoint called by the INSTANCE_CREATE worker handler before issuing CreateInstance
// to the Host Agent.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"

	_ "github.com/lib/pq"
)

// Allocator performs atomic IP allocation using database transactions.
type Allocator struct {
	db  *sql.DB
	log *slog.Logger
}

// NewAllocator constructs an Allocator connected to the given database.
func NewAllocator(db *sql.DB, log *slog.Logger) *Allocator {
	return &Allocator{db: db, log: log}
}

// Allocate atomically claims an available IP for an instance within the given VPC.
//
// Uses SELECT FOR UPDATE SKIP LOCKED so concurrent callers never block each other —
// each caller skips rows locked by another transaction and picks the next free IP.
// This eliminates the lock-contention bottleneck that a plain SELECT FOR UPDATE would cause.
//
// Returns (ip, nil) on success.
// Returns ("", ErrNoAvailableIPs) when the pool is exhausted.
//
// Source: IP_ALLOCATION_CONTRACT_V1 §2 (atomic allocation transaction).
func (a *Allocator) Allocate(ctx context.Context, vpcID, instanceID string) (string, error) {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return "", fmt.Errorf("Allocate: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Step 1: Lock a free IP row, skipping rows locked by concurrent transactions.
	var ip string
	err = tx.QueryRowContext(ctx, `
		SELECT ip_address
		FROM ip_allocations
		WHERE vpc_id    = $1
		  AND allocated = FALSE
		ORDER BY ip_address
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`, vpcID).Scan(&ip)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("Allocate: no available IPs in vpc %s", vpcID)
		}
		return "", fmt.Errorf("Allocate: select: %w", err)
	}

	// Step 2: Claim the locked row.
	tag, err := tx.ExecContext(ctx, `
		UPDATE ip_allocations
		SET allocated         = TRUE,
		    owner_instance_id = $3
		WHERE ip_address = $1
		  AND vpc_id     = $2
		  AND allocated  = FALSE
	`, ip, vpcID, instanceID)
	if err != nil {
		return "", fmt.Errorf("Allocate: update: %w", err)
	}
	rows, _ := tag.RowsAffected()
	if rows == 0 {
		// Another concurrent transaction beat us to it — should not happen
		// with SKIP LOCKED, but guard defensively.
		return "", fmt.Errorf("Allocate: race on ip %s in vpc %s", ip, vpcID)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("Allocate: commit: %w", err)
	}

	a.log.Info("IP allocated",
		"ip", ip,
		"vpc_id", vpcID,
		"instance_id", instanceID,
	)
	return ip, nil
}

// handleAllocate is the HTTP handler for POST /internal/v1/ip/allocate.
// Request body (JSON): { "vpc_id": "...", "instance_id": "..." }
// Response (JSON):     { "ip": "10.0.0.x" }
func (a *Allocator) handleAllocate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		VPCID      string `json:"vpc_id"`
		InstanceID string `json:"instance_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.VPCID == "" || req.InstanceID == "" {
		http.Error(w, "vpc_id and instance_id are required", http.StatusBadRequest)
		return
	}

	ip, err := a.Allocate(r.Context(), req.VPCID, req.InstanceID)
	if err != nil {
		a.log.Warn("IP allocation failed", "vpc_id", req.VPCID, "instance_id", req.InstanceID, "error", err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"ip": ip})
}
