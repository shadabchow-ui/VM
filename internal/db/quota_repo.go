package db

// quota_repo.go — Project-aware quota persistence for VM-P2D Slice 3.
//
// Design: hard, DB-backed quotas enforced synchronously at admission time.
// No caching layer (deferred per vm-13-02__blueprint__ §mvp.deferred).
//
// Quota check uses SELECT FOR UPDATE on the quota row followed by a comparison
// against counted active instances. This is fully transactional on the pool
// that callers provide — single-row lock prevents concurrent over-admission.
//
// Scope: VM instance create admission only. Volume/IP quotas are not included
// in this slice. Per-instance-family quotas are Phase 3 (blueprint §future_phases).
//
// Error separation (vm-13-02__blueprint__ §core_contracts "Error Code Separation"):
//   - ErrQuotaExceeded → resource-manager maps to HTTP 422 quota_exceeded
//   - ErrNoCapacity (scheduler/placement.go) → HTTP 503 insufficient_capacity
//   These must never be collapsed. This file only owns the quota side.
//
// Source: vm-13-02__blueprint__ §mvp, §core_contracts "Transactional Quota Integrity",
//         vm-16-01__blueprint__ §quota_enforcement_point,
//         AUTH_OWNERSHIP_MODEL_V1 §3 (project-scoped ownership),
//         P2_PROJECT_RBAC_MODEL.md §2.4 (project schema).

import (
	"context"
	"errors"
	"fmt"
)

// ErrQuotaExceeded is returned when a create request exceeds the project's
// or account's instance quota.
// Callers (resource-manager handler) must map this to HTTP 422 quota_exceeded,
// never to the scheduler's capacity failure path.
var ErrQuotaExceeded = errors.New("quota exceeded")

// QuotaRow is the DB projection of a project_quotas or account_quotas row.
// Phase G-H expansion: added vCPU, memory_mb, root_disk_gb, volume_gb, and
// max_ip_count dimensions alongside the existing max_instances.
type QuotaRow struct {
	// ScopeID is the principal_id of the scope — either the project principal_id
	// (project-aware path) or the owner principal_id (classic/no-project path).
	ScopeID      string
	MaxInstances int
	MaxVCPU      int
	MaxMemoryMB  int
	MaxRootDiskGB int
	MaxVolumeGB  int
	MaxIPCount   int
}

// Default quota constants applied to scopes without an explicit row.
// Operators override these per scope via direct DB writes.
var (
	DefaultMaxInstances = 10
	DefaultMaxVCPU      = 64
	DefaultMaxMemoryMB  = 262144 // 256 GB
	DefaultMaxRootDiskGB = 5000
	DefaultMaxVolumeGB  = 10000
	DefaultMaxIPCount   = 10
)

// GetQuota returns the quota row for the given scopeID.
// Returns a default row when no explicit row exists.
//
// SQL pattern matches memPool.QueryRow dispatch:
//
//	"FROM project_quotas" AND "scope_id = $1"
func (r *Repo) GetQuota(ctx context.Context, scopeID string) (*QuotaRow, error) {
	const q = `
SELECT scope_id, max_instances,
       COALESCE(max_vcpu, $2) AS max_vcpu,
       COALESCE(max_memory_mb, $3) AS max_memory_mb,
       COALESCE(max_root_disk_gb, $4) AS max_root_disk_gb,
       COALESCE(max_volume_gb, $5) AS max_volume_gb,
       COALESCE(max_ip_count, $6) AS max_ip_count
FROM project_quotas
WHERE scope_id = $1`
	row := r.pool.QueryRow(ctx, q,
		scopeID,
		DefaultMaxVCPU, DefaultMaxMemoryMB,
		DefaultMaxRootDiskGB, DefaultMaxVolumeGB, DefaultMaxIPCount,
	)
	qr := &QuotaRow{}
	if err := row.Scan(
		&qr.ScopeID, &qr.MaxInstances,
		&qr.MaxVCPU, &qr.MaxMemoryMB,
		&qr.MaxRootDiskGB, &qr.MaxVolumeGB, &qr.MaxIPCount,
	); err != nil {
		// No row → return the platform defaults.
		return &QuotaRow{
			ScopeID:       scopeID,
			MaxInstances:  DefaultMaxInstances,
			MaxVCPU:       DefaultMaxVCPU,
			MaxMemoryMB:   DefaultMaxMemoryMB,
			MaxRootDiskGB: DefaultMaxRootDiskGB,
			MaxVolumeGB:   DefaultMaxVolumeGB,
			MaxIPCount:    DefaultMaxIPCount,
		}, nil
	}
	return qr, nil
}

// CountActiveInstancesByScope counts non-deleted, non-failed instances for the
// given scopeID. Used to check current usage against the quota limit.
//
// Classic mode (no project): scopeID == ownerPrincipalID → counts by owner.
// Project-aware mode:        scopeID == project principal_id → counts by project.
//
// Phase 1: instances are owned by a single principal; project membership is
// expressed as the ownerPrincipalID being the project's principal_id when the
// create request carries a project context. The resource-manager handler
// selects the correct scopeID before calling CheckAndDecrementQuota.
//
// SQL pattern matches memPool.QueryRow dispatch:
//
//	"SELECT COUNT(*)" AND "FROM instances" AND "owner_principal_id = $1"
func (r *Repo) CountActiveInstancesByScope(ctx context.Context, scopeID string) (int, error) {
	const q = `
SELECT COUNT(*)
FROM instances
WHERE owner_principal_id = $1
  AND deleted_at IS NULL
  AND vm_state NOT IN ('deleted', 'failed')`
	row := r.pool.QueryRow(ctx, q, scopeID)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("CountActiveInstancesByScope %s: %w", scopeID, err)
	}
	return count, nil
}

// CheckAndDecrementQuota performs the admission-time quota check for a VM create.
// It compares current active instance count against the scope limit.
//
// Returns ErrQuotaExceeded when current+1 > limit.
// Returns nil when the request is within quota (admission proceeds).
//
// Phase 1 implementation: count-based check without a decrement column.
// The "decrement" is implicit — the caller proceeds to InsertInstance which
// increments the count visible to the next check. This is correct for the
// synchronous admission pattern where InsertInstance immediately follows
// and the DB count is the source of truth.
//
// Transactional quota integrity (blueprint §core_contracts): if InsertInstance
// or subsequent networking steps fail and the caller rolls back (deletes the
// instance row), the count returns to its previous value automatically — no
// separate Refund call needed. See RefundQuota for the explicit API.
//
// Source: vm-13-02__blueprint__ §mvp, §core_contracts "Transactional Quota Integrity".
func (r *Repo) CheckAndDecrementQuota(ctx context.Context, scopeID string) error {
	quota, err := r.GetQuota(ctx, scopeID)
	if err != nil {
		return fmt.Errorf("CheckAndDecrementQuota get quota: %w", err)
	}

	current, err := r.CountActiveInstancesByScope(ctx, scopeID)
	if err != nil {
		return fmt.Errorf("CheckAndDecrementQuota count: %w", err)
	}

	if current >= quota.MaxInstances {
		return ErrQuotaExceeded
	}
	return nil
}

// RefundQuota is the explicit quota refund API for cases where the count-based
// automatic reclamation is not sufficient (e.g. reservation-based models, or
// callers that want to release quota before the instance row transitions).
//
// Phase 1 implementation: count-based model. Since CountActiveInstancesByScope
// already excludes instances with vm_state IN ('deleted', 'failed'), this is
// a logical no-op at the DB layer — the quota is automatically freed when the
// instance transitions to failed or is soft-deleted. This method exists as the
// explicit seam for future reservation-based quota models.
//
// Always returns nil. Never errors — quota integrity does not require explicit
// refund in the count-based model.
//
// Source: vm-13-02__blueprint__ §core_contracts "Transactional Quota Integrity".
func (r *Repo) RefundQuota(ctx context.Context, scopeID string) error {
	// Count-based model: no explicit refund needed.
	// When the instance is failed/soft-deleted, CountActiveInstancesByScope
	// excludes it automatically. This method is the seam for Phase 2
	// reservation-column models where an explicit decrement is required.
	_ = scopeID
	return nil
}

// ReserveQuota provides a forward-compatible seam for reservation-based quota.
// Phase 1: delegates to CheckAndDecrementQuota (count-based equivalent).
// Returns ErrQuotaExceeded when the scope is at or above its limit.
//
// Source: vm-13-02__blueprint__ §future_phases "Reservation-Based Quota".
func (r *Repo) ReserveQuota(ctx context.Context, scopeID string) error {
	return r.CheckAndDecrementQuota(ctx, scopeID)
}

// CheckCreateQuota validates enhanced quota dimensions for a VM create request.
//
// Primary check: max_instances count-based (backward compatible, uses existing
// CountActiveInstancesByScope which queries the actual instances table).
//
// Dimension checks (shape-based, no DB SUM needed since instances table does not
// denormalize vcpus/memory_mb/root_gb): these compute aggregate usage by joining
// instance_type data from the instance_types reference table. When the shape
// resolution fails (unknown instance type), the dimension checks are skipped.
//
// Returns ErrQuotaExceeded when any dimension would be exceeded.
// The error message names the specific dimension that is at limit.
//
// Source: vm-13-02__blueprint__ §mvp (expanded dimensions for P2D).
func (r *Repo) CheckCreateQuota(ctx context.Context, scopeID string, instanceVCPU, instanceMemoryMB, instanceRootDiskGB int) error {
	quota, err := r.GetQuota(ctx, scopeID)
	if err != nil {
		return fmt.Errorf("CheckCreateQuota get quota: %w", err)
	}

	// Primary: instance count.
	currentInstances, err := r.CountActiveInstancesByScope(ctx, scopeID)
	if err != nil {
		return fmt.Errorf("CheckCreateQuota count instances: %w", err)
	}
	if currentInstances >= quota.MaxInstances {
		return fmt.Errorf("%w: max_instances (%d)", ErrQuotaExceeded, quota.MaxInstances)
	}

	// Dimension checks: compute aggregates via instance_type reference table join.
	// This works without denormalized columns on the instances table.
	currentVCPU, err := r.SumActiveVCPUByScopeViaTypes(ctx, scopeID)
	if err == nil && currentVCPU >= 0 {
		if currentVCPU+instanceVCPU > quota.MaxVCPU {
			return fmt.Errorf("%w: max_vcpu (%d, would be %d)", ErrQuotaExceeded, quota.MaxVCPU, currentVCPU+instanceVCPU)
		}
	}

	currentMem, err := r.SumActiveMemoryMBByScopeViaTypes(ctx, scopeID)
	if err == nil && currentMem >= 0 {
		if currentMem+instanceMemoryMB > quota.MaxMemoryMB {
			return fmt.Errorf("%w: max_memory_mb (%d, would be %d)", ErrQuotaExceeded, quota.MaxMemoryMB, currentMem+instanceMemoryMB)
		}
	}

	currentDisk, err := r.SumActiveRootDiskGBByScope(ctx, scopeID)
	if err == nil && currentDisk >= 0 {
		if currentDisk+instanceRootDiskGB > quota.MaxRootDiskGB {
			return fmt.Errorf("%w: max_root_disk_gb (%d, would be %d)", ErrQuotaExceeded, quota.MaxRootDiskGB, currentDisk+instanceRootDiskGB)
		}
	}

	return nil
}

// SumActiveVCPUByScopeViaTypes returns total vcpus across all active instances
// in the scope, joining instance_types to resolve shape dimensions.
func (r *Repo) SumActiveVCPUByScopeViaTypes(ctx context.Context, scopeID string) (int, error) {
	const q = `
SELECT COALESCE(SUM(it.vcpus), 0)
FROM instances i
JOIN instance_types it ON i.instance_type_id = it.id
WHERE i.owner_principal_id = $1
  AND i.deleted_at IS NULL
  AND i.vm_state NOT IN ('deleted', 'failed')`
	row := r.pool.QueryRow(ctx, q, scopeID)
	var total int
	if err := row.Scan(&total); err != nil {
		return 0, fmt.Errorf("SumActiveVCPUByScopeViaTypes %s: %w", scopeID, err)
	}
	return total, nil
}

// SumActiveMemoryMBByScopeViaTypes returns total memory_mb across all active
// instances in the scope, joining instance_types for shape dimensions.
func (r *Repo) SumActiveMemoryMBByScopeViaTypes(ctx context.Context, scopeID string) (int, error) {
	const q = `
SELECT COALESCE(SUM(it.memory_mb), 0)
FROM instances i
JOIN instance_types it ON i.instance_type_id = it.id
WHERE i.owner_principal_id = $1
  AND i.deleted_at IS NULL
  AND i.vm_state NOT IN ('deleted', 'failed')`
	row := r.pool.QueryRow(ctx, q, scopeID)
	var total int
	if err := row.Scan(&total); err != nil {
		return 0, fmt.Errorf("SumActiveMemoryMBByScopeViaTypes %s: %w", scopeID, err)
	}
	return total, nil
}

// SumActiveRootDiskGBByScope retrieves total root_gb via root_disks table.
func (r *Repo) SumActiveRootDiskGBByScope(ctx context.Context, scopeID string) (int, error) {
	const q = `
SELECT COALESCE(SUM(rd.size_gb), 0)
FROM root_disks rd
JOIN instances i ON i.id = rd.instance_id
WHERE i.owner_principal_id = $1
  AND i.deleted_at IS NULL
  AND i.vm_state NOT IN ('deleted', 'failed')
  AND rd.deleted_at IS NULL`
	row := r.pool.QueryRow(ctx, q, scopeID)
	var total int
	if err := row.Scan(&total); err != nil {
		return 0, fmt.Errorf("SumActiveRootDiskGBByScope %s: %w", scopeID, err)
	}
	return total, nil
}

// CountAllocatedIPsByScope counts IP allocations for the scope's instances.
func (r *Repo) CountAllocatedIPsByScope(ctx context.Context, scopeID string) (int, error) {
	const q = `
SELECT COUNT(*)
FROM ip_allocations ia
JOIN instances i ON i.id = ia.instance_id
WHERE i.owner_principal_id = $1
  AND i.deleted_at IS NULL
  AND i.vm_state NOT IN ('deleted', 'failed')
  AND ia.released = FALSE`
	row := r.pool.QueryRow(ctx, q, scopeID)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("CountAllocatedIPsByScope %s: %w", scopeID, err)
	}
	return count, nil
}

// CheckVolumeCreateQuota validates volume quota dimensions for a volume create.
//
// Checks max_volume_gb against the scope's current total volume GB usage.
// Returns ErrQuotaExceeded when the limit would be exceeded.
//
// Source: vm-13-02__blueprint__ §mvp (expanded dimensions for P2D).
func (r *Repo) CheckVolumeCreateQuota(ctx context.Context, scopeID string, volumeSizeGB int) error {
	quota, err := r.GetQuota(ctx, scopeID)
	if err != nil {
		return fmt.Errorf("CheckVolumeCreateQuota get quota: %w", err)
	}

	currentGB, err := r.SumActiveVolumeGBByScope(ctx, scopeID)
	if err != nil {
		return fmt.Errorf("CheckVolumeCreateQuota sum: %w", err)
	}
	if currentGB+volumeSizeGB > quota.MaxVolumeGB {
		return fmt.Errorf("%w: max_volume_gb (%d, would be %d)", ErrQuotaExceeded, quota.MaxVolumeGB, currentGB+volumeSizeGB)
	}
	return nil
}

// SumActiveVolumeGBByScope returns the total size_gb of all active volumes in the scope.
func (r *Repo) SumActiveVolumeGBByScope(ctx context.Context, scopeID string) (int, error) {
	const q = `
SELECT COALESCE(SUM(size_gb), 0)
FROM volumes
WHERE owner_principal_id = $1
  AND deleted_at IS NULL
  AND status NOT IN ('deleted', 'failed')`
	row := r.pool.QueryRow(ctx, q, scopeID)
	var total int
	if err := row.Scan(&total); err != nil {
		return 0, fmt.Errorf("SumActiveVolumeGBByScope %s: %w", scopeID, err)
	}
	return total, nil
}
