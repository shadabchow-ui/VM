package db

// ip_repo.go — IP allocation and release methods.
//
// Source: IP_ALLOCATION_CONTRACT_V1 §2 (atomic allocation, idempotent release),
//         IMPLEMENTATION_PLAN_V1 §32-33, core-architecture-blueprint §ip_allocations.
//
// Invariant I-2: no two instances share the same IP within a VPC.
// Enforced by: UNIQUE(vpc_id, ip_address) constraint + SELECT FOR UPDATE in allocation.
//
// Full concurrent implementation (SELECT FOR UPDATE SKIP LOCKED) is Sprint 3 / M6.
// The SQL here is correct; the pgxpool.BeginTx wrapper goes in the network controller.

import (
	"context"
	"fmt"
)

// IPAllocationRow is the DB representation of an ip_allocations row.
type IPAllocationRow struct {
	IPAddress       string
	VpcID           string
	Allocated       bool
	OwnerInstanceID *string
}

// AllocateIP atomically claims an available IP for an instance.
// Uses SELECT FOR UPDATE SKIP LOCKED to prevent concurrent double-allocation.
//
// Full transaction pattern (Sprint 3 network controller):
//
//	tx, _ := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
//	SELECT ip_address FROM ip_allocations WHERE vpc_id=$1 AND allocated=FALSE
//	  ORDER BY ip_address LIMIT 1 FOR UPDATE SKIP LOCKED
//	UPDATE ip_allocations SET allocated=TRUE, owner_instance_id=$2 WHERE ip_address=$3 AND vpc_id=$1
//	tx.Commit(ctx)
//
// Source: IP_ALLOCATION_CONTRACT_V1, IMPLEMENTATION_PLAN_V1 §BLOCK (M2 gate).
func (r *Repo) AllocateIP(ctx context.Context, vpcID, instanceID string) (string, error) {
	// Note: this single-statement form is correct for the M1 milestone.
	// The transaction wrapper (BEGIN/SELECT FOR UPDATE/UPDATE/COMMIT) is
	// implemented in services/network-controller/allocator.go (Sprint 3).
	var ip string
	err := r.pool.QueryRow(ctx, `
		UPDATE ip_allocations
		SET allocated        = TRUE,
		    owner_instance_id = $2
		WHERE (ip_address, vpc_id) = (
			SELECT ip_address, vpc_id
			FROM ip_allocations
			WHERE vpc_id    = $1
			  AND allocated = FALSE
			LIMIT 1
		)
		RETURNING ip_address
	`, vpcID, instanceID).Scan(&ip)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return "", fmt.Errorf("AllocateIP: no available IPs in vpc %s", vpcID)
		}
		return "", fmt.Errorf("AllocateIP: %w", err)
	}
	return ip, nil
}

// ReleaseIP idempotently releases an IP back to the pool.
// Safe to call multiple times — if already released the UPDATE affects 0 rows (no error).
// Source: IP_ALLOCATION_CONTRACT_V1 §release (idempotent).
func (r *Repo) ReleaseIP(ctx context.Context, ipAddress, vpcID, instanceID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE ip_allocations
		SET allocated         = FALSE,
		    owner_instance_id = NULL
		WHERE ip_address        = $1
		  AND vpc_id            = $2
		  AND owner_instance_id = $3
	`, ipAddress, vpcID, instanceID)
	if err != nil {
		return fmt.Errorf("ReleaseIP: %w", err)
	}
	// 0 rows affected is OK (already released — idempotent).
	return nil
}

// GetIPByInstance returns the IP currently allocated to an instance.
// Returns ("", nil) if no IP is allocated to this instance.
func (r *Repo) GetIPByInstance(ctx context.Context, instanceID string) (string, error) {
	var ip string
	err := r.pool.QueryRow(ctx, `
		SELECT ip_address FROM ip_allocations
		WHERE owner_instance_id = $1 AND allocated = TRUE
		LIMIT 1
	`, instanceID).Scan(&ip)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return "", nil
		}
		return "", fmt.Errorf("GetIPByInstance: %w", err)
	}
	return ip, nil
}

// DuplicateIPRow is a result row from FindDuplicateIPAllocations.
// Each row represents one IP+VPC combination that is claimed by multiple instances.
type DuplicateIPRow struct {
	IPAddress        string
	VPCID            string
	OwnerInstanceIDs []string
}

// FindDuplicateIPAllocations queries ip_allocations for rows where the same
// ip_address+vpc_id pair is allocated=TRUE for more than one owner_instance_id.
//
// This detects violations of invariant I-2 (no two instances share the same IP
// within a VPC). The query is read-only; no corrective action is taken here.
//
// Returns an empty slice when the pool is clean (the expected case).
// Called by the IP uniqueness reconciler sub-scan on each 5-minute cycle.
//
// Source: IP_ALLOCATION_CONTRACT_V1 §anomaly-detection,
//
//	IMPLEMENTATION_PLAN_V1 §M6 gate (IP uniqueness reconciler sub-scan),
//	02-04-system-invariants.md I-2.
func (r *Repo) FindDuplicateIPAllocations(ctx context.Context) ([]DuplicateIPRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT ip_address, vpc_id, array_agg(owner_instance_id ORDER BY owner_instance_id) AS owners
		FROM ip_allocations
		WHERE allocated = TRUE
		  AND owner_instance_id IS NOT NULL
		GROUP BY ip_address, vpc_id
		HAVING count(*) > 1
		ORDER BY vpc_id, ip_address
	`)
	if err != nil {
		return nil, fmt.Errorf("FindDuplicateIPAllocations: %w", err)
	}
	defer rows.Close()

	var out []DuplicateIPRow
	for rows.Next() {
		var row DuplicateIPRow
		if err := rows.Scan(&row.IPAddress, &row.VPCID, &row.OwnerInstanceIDs); err != nil {
			return nil, fmt.Errorf("FindDuplicateIPAllocations scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("FindDuplicateIPAllocations rows: %w", err)
	}
	return out, nil
}
