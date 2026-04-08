package main

// releaser.go — Idempotent IP release back to the pool.
//
// Source: IP_ALLOCATION_CONTRACT_V1 §release, IMPLEMENTATION_PLAN_V1 §33.
//
// Release is a compensating action for IP allocation. It must be:
//   - Idempotent: releasing an already-released IP returns nil (no error).
//   - Safe to call multiple times from rollback logic.
//   - Owner-scoped: only the instance that holds the IP can release it.
//
// SQL: UPDATE ip_allocations SET allocated=FALSE, owner_instance_id=NULL
//      WHERE ip_address=$1 AND vpc_id=$2 AND owner_instance_id=$3
//
// 0 rows affected = already released or different owner → not an error.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
)

// Releaser performs idempotent IP release.
type Releaser struct {
	db  *sql.DB
	log *slog.Logger
}

// NewReleaser constructs a Releaser.
func NewReleaser(db *sql.DB, log *slog.Logger) *Releaser {
	return &Releaser{db: db, log: log}
}

// Release idempotently returns ipAddress to the free pool for the given instance.
// It is safe to call multiple times — if the IP is already free or belongs to
// a different instance, the UPDATE affects 0 rows and nil is returned.
//
// Source: IP_ALLOCATION_CONTRACT_V1 §release (idempotent compensating action).
func (rel *Releaser) Release(ctx context.Context, ipAddress, vpcID, instanceID string) error {
	tag, err := rel.db.ExecContext(ctx, `
		UPDATE ip_allocations
		SET allocated         = FALSE,
		    owner_instance_id = NULL
		WHERE ip_address        = $1
		  AND vpc_id            = $2
		  AND owner_instance_id = $3
	`, ipAddress, vpcID, instanceID)
	if err != nil {
		return fmt.Errorf("Release: %w", err)
	}
	rows, _ := tag.RowsAffected()
	if rows == 0 {
		// Already released or different owner — idempotent no-op.
		rel.log.Info("IP release: already free or different owner — no-op",
			"ip", ipAddress,
			"vpc_id", vpcID,
			"instance_id", instanceID,
		)
		return nil
	}

	rel.log.Info("IP released",
		"ip", ipAddress,
		"vpc_id", vpcID,
		"instance_id", instanceID,
	)
	return nil
}

// handleRelease is the HTTP handler for POST /internal/v1/ip/release.
// Request body (JSON): { "ip": "10.0.0.x", "vpc_id": "...", "instance_id": "..." }
// Response: 204 No Content on success.
func (rel *Releaser) handleRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IP         string `json:"ip"`
		VPCID      string `json:"vpc_id"`
		InstanceID string `json:"instance_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.IP == "" || req.VPCID == "" || req.InstanceID == "" {
		http.Error(w, "ip, vpc_id, and instance_id are required", http.StatusBadRequest)
		return
	}

	if err := rel.Release(r.Context(), req.IP, req.VPCID, req.InstanceID); err != nil {
		rel.log.Error("IP release failed", "ip", req.IP, "instance_id", req.InstanceID, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
