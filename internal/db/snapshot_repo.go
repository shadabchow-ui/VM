package db

// snapshot_repo.go — Snapshot persistence methods.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.8 (schema), §2.4 (state machine),
//         §2.9 (invariants), vm-15-02__skill__snapshot-clone-restore-retention-model.md.
//
// VM-P2B-S2: first-class snapshot resource persistence.
//
// Ownership model: snapshots carry owner_principal_id. All list/get operations
// that surface snapshots to API callers must filter by owner. Cross-account
// reads return nil/empty — the handler enforces 404 (not 403).
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
//
// Locking model: same optimistic lock pattern as volumes.
// locked_by column + version increment on every status change.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-1.

import (
	"context"
	"fmt"
	"time"
)

// ── SnapshotRow ───────────────────────────────────────────────────────────────

// SnapshotRow is the DB representation of a snapshot record.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.8.
type SnapshotRow struct {
	ID               string
	OwnerPrincipalID string
	DisplayName      string
	Region           string
	SourceVolumeID   *string // nullable
	SourceInstanceID *string // nullable
	SizeGB           int
	Status           string // see SnapshotStatus constants in domain-model
	ProgressPercent  int
	StoragePath      *string // set after creation completes
	StoragePoolID    *string
	Encrypted        bool
	Version          int
	LockedBy         *string // job_id holding exclusive mutation lock
	CreatedAt        time.Time
	CompletedAt      *time.Time // set when status → available
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// Snapshot status constants — mirror domain-model/snapshot.go for use at DB
// layer without importing the domain package.
const (
	SnapshotStatusPending   = "pending"
	SnapshotStatusCreating  = "creating"
	SnapshotStatusAvailable = "available"
	SnapshotStatusError     = "error"
	SnapshotStatusDeleting  = "deleting"
	SnapshotStatusDeleted   = "deleted"
)

// ── Snapshot CRUD ─────────────────────────────────────────────────────────────

// CreateSnapshot inserts a new snapshot record in 'pending' status.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.4 (initial state: pending), §2.8.
func (r *Repo) CreateSnapshot(ctx context.Context, row *SnapshotRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO snapshots (
			id, owner_principal_id, display_name, region,
			source_volume_id, source_instance_id,
			size_gb, status, progress_percent, encrypted,
			storage_pool_id, version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',0,$8,$9,0,NOW(),NOW())
	`,
		row.ID, row.OwnerPrincipalID, row.DisplayName, row.Region,
		row.SourceVolumeID, row.SourceInstanceID,
		row.SizeGB, row.Encrypted,
		row.StoragePoolID,
	)
	if err != nil {
		return fmt.Errorf("CreateSnapshot: %w", err)
	}
	return nil
}

// GetSnapshotByID fetches a single snapshot by primary key.
// Returns nil, nil when no matching row exists.
// Does NOT filter by owner — caller must enforce ownership.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func (r *Repo) GetSnapshotByID(ctx context.Context, id string) (*SnapshotRow, error) {
	row := &SnapshotRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_principal_id, display_name, region,
		       source_volume_id, source_instance_id,
		       size_gb, status, progress_percent,
		       storage_path, storage_pool_id, encrypted,
		       version, locked_by, created_at, completed_at, updated_at, deleted_at
		FROM snapshots
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id).Scan(
		&row.ID, &row.OwnerPrincipalID, &row.DisplayName, &row.Region,
		&row.SourceVolumeID, &row.SourceInstanceID,
		&row.SizeGB, &row.Status, &row.ProgressPercent,
		&row.StoragePath, &row.StoragePoolID, &row.Encrypted,
		&row.Version, &row.LockedBy, &row.CreatedAt, &row.CompletedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSnapshotByID: %w", err)
	}
	return row, nil
}

// ListSnapshotsByOwner returns all non-deleted snapshots for a principal, newest first.
// Source: AUTH_OWNERSHIP_MODEL_V1 §4, P2_IMAGE_SNAPSHOT_MODEL.md §4.
func (r *Repo) ListSnapshotsByOwner(ctx context.Context, ownerPrincipalID string) ([]*SnapshotRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_principal_id, display_name, region,
		       source_volume_id, source_instance_id,
		       size_gb, status, progress_percent,
		       storage_path, storage_pool_id, encrypted,
		       version, locked_by, created_at, completed_at, updated_at, deleted_at
		FROM snapshots
		WHERE owner_principal_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, ownerPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("ListSnapshotsByOwner: %w", err)
	}
	defer rows.Close()

	var out []*SnapshotRow
	for rows.Next() {
		row := &SnapshotRow{}
		if err := rows.Scan(
			&row.ID, &row.OwnerPrincipalID, &row.DisplayName, &row.Region,
			&row.SourceVolumeID, &row.SourceInstanceID,
			&row.SizeGB, &row.Status, &row.ProgressPercent,
			&row.StoragePath, &row.StoragePoolID, &row.Encrypted,
			&row.Version, &row.LockedBy, &row.CreatedAt, &row.CompletedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListSnapshotsByOwner scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateSnapshotStatus transitions a snapshot to a new status using optimistic locking.
// Returns an error if 0 rows were updated (concurrent modification or stale version).
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5.
func (r *Repo) UpdateSnapshotStatus(ctx context.Context, id, expectedStatus, newStatus string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE snapshots
		SET status     = $3,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id      = $1
		  AND status  = $2
		  AND version = $4
		  AND deleted_at IS NULL
	`, id, expectedStatus, newStatus, version)
	if err != nil {
		return fmt.Errorf("UpdateSnapshotStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateSnapshotStatus: concurrent modification or status mismatch for snapshot %s", id)
	}
	return nil
}

// MarkSnapshotAvailable transitions creating → available and sets storage_path +
// completed_at + progress_percent=100 atomically.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5, SNAP-I-1.
func (r *Repo) MarkSnapshotAvailable(ctx context.Context, id, storagePath string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE snapshots
		SET status           = 'available',
		    storage_path     = $2,
		    progress_percent = 100,
		    completed_at     = NOW(),
		    locked_by        = NULL,
		    version          = version + 1,
		    updated_at       = NOW()
		WHERE id      = $1
		  AND status  = 'creating'
		  AND version = $3
		  AND deleted_at IS NULL
	`, id, storagePath, version)
	if err != nil {
		return fmt.Errorf("MarkSnapshotAvailable: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("MarkSnapshotAvailable: concurrent modification for snapshot %s", id)
	}
	return nil
}

// LockSnapshot acquires the mutation lock on a snapshot.
// Uses optimistic locking: only succeeds if version matches and locked_by IS NULL.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-1.
func (r *Repo) LockSnapshot(ctx context.Context, id, jobID, expectedStatus string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE snapshots
		SET locked_by  = $2,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id        = $1
		  AND status    = $3
		  AND version   = $4
		  AND locked_by IS NULL
		  AND deleted_at IS NULL
	`, id, jobID, expectedStatus, version)
	if err != nil {
		return fmt.Errorf("LockSnapshot: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("LockSnapshot: snapshot %s is locked or version mismatch", id)
	}
	return nil
}

// UnlockSnapshot releases the mutation lock and transitions status in a single update.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-1.
func (r *Repo) UnlockSnapshot(ctx context.Context, id, newStatus string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE snapshots
		SET locked_by  = NULL,
		    status     = $2,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id, newStatus)
	if err != nil {
		return fmt.Errorf("UnlockSnapshot: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UnlockSnapshot: snapshot %s not found", id)
	}
	return nil
}

// SoftDeleteSnapshot marks a snapshot as deleted.
// Called after SNAPSHOT_DELETE job completes successfully.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5.
func (r *Repo) SoftDeleteSnapshot(ctx context.Context, id string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE snapshots
		SET status     = 'deleted',
		    deleted_at = NOW(),
		    locked_by  = NULL,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id      = $1
		  AND version = $2
		  AND deleted_at IS NULL
	`, id, version)
	if err != nil {
		return fmt.Errorf("SoftDeleteSnapshot: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteSnapshot: concurrent modification for snapshot %s", id)
	}
	return nil
}

// HasActiveSnapshotJob returns true when the snapshot already has a pending or
// in_progress job of the given type.
// Source: JOB_MODEL_V1 §idempotency, P2_IMAGE_SNAPSHOT_MODEL.md §2.9.
func (r *Repo) HasActiveSnapshotJob(ctx context.Context, snapshotID, jobType string) (bool, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM jobs
		WHERE snapshot_id = $1
		  AND job_type    = $2
		  AND status IN ('pending', 'in_progress')
	`, snapshotID, jobType).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("HasActiveSnapshotJob: %w", err)
	}
	return count > 0, nil
}

// CountActiveSnapshotsByVolume returns the number of non-deleted snapshots whose
// source_volume_id matches the given volume.
// Used to enforce SNAP-I-3: cannot delete a volume that has active snapshots
// (conservative Phase 2 rule — prevents dangling CoW chains).
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.7, §2.9 SNAP-I-3.
func (r *Repo) CountActiveSnapshotsByVolume(ctx context.Context, volumeID string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM snapshots
		WHERE source_volume_id = $1
		  AND status NOT IN ('deleted')
		  AND deleted_at IS NULL
	`, volumeID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountActiveSnapshotsByVolume: %w", err)
	}
	return count, nil
}

// InsertSnapshotJob inserts a job scoped to a snapshot.
// For SNAPSHOT_CREATE and SNAPSHOT_DELETE jobs, only snapshot_id is set.
// For VOLUME_RESTORE jobs, both snapshot_id and volume_id are set so the
// worker can locate the destination volume without a separate scan.
// ON CONFLICT on idempotency_key does nothing — caller checks for existing job.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (async job dispatch).
func (r *Repo) InsertSnapshotJob(ctx context.Context, row *JobRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, snapshot_id, volume_id, job_type, status,
			idempotency_key, attempt_count, max_attempts,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,'pending',$5,0,$6,NOW(),NOW())
		ON CONFLICT (idempotency_key) DO NOTHING
	`,
		row.ID, row.SnapshotID, row.VolumeID, row.JobType,
		row.IdempotencyKey, row.MaxAttempts,
	)
	if err != nil {
		return fmt.Errorf("InsertSnapshotJob: %w", err)
	}
	return nil
}
