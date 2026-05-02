package db

// volume_repo.go — Volume and VolumeAttachment persistence methods.
//
// Source: P2_VOLUME_MODEL.md §6 (schema), §3 (state machine), §4 (attachment semantics),
//         §7 (invariants), vm-15-01__skill__independent-block-volume-architecture.md.
//
// VM-P2B Slice 1: first-class independent block volume persistence.
// VM-P2B-S3: Added SetVolumeStoragePath for use by VOLUME_CREATE and VOLUME_RESTORE workers.
//
// Ownership model: volumes carry owner_principal_id. All list/get operations
// that surface volumes to API callers must filter by owner. Cross-account reads
// return nil/empty — the handler enforces the 404 response (not 403).
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
//
// Locking model: VOL-I-5 — at most one state-mutating job per volume at a time.
// Enforced via the locked_by column + optimistic version increment on status change.
// Same pattern as instances (version column + WHERE version = $n).
// Source: P2_VOLUME_MODEL.md §7 (VOL-I-5), core-architecture-blueprint §optimistic locking.

import (
	"context"
	"fmt"
	"time"
)

// ── VolumeRow ─────────────────────────────────────────────────────────────────

// VolumeRow is the DB representation of a volume record.
// Source: P2_VOLUME_MODEL.md §6.
type VolumeRow struct {
	ID               string
	OwnerPrincipalID string
	DisplayName      string
	Region           string
	AvailabilityZone string
	SizeGB           int
	Origin           string  // 'blank' | 'root_disk' | 'snapshot'
	SourceDiskID     *string // non-nil for origin='root_disk'
	SourceSnapshotID *string // non-nil for origin='snapshot'
	Status           string  // see VolumeStatus constants in domain-model
	StoragePath      *string // set after creation completes
	StoragePoolID    *string
	Version          int
	LockedBy         *string // job_id holding exclusive mutation lock
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// Volume status constants — mirror domain-model/volume.go for use at DB layer
// without importing the domain package from internal/db.
const (
	VolumeStatusCreating  = "creating"
	VolumeStatusAvailable = "available"
	VolumeStatusAttaching = "attaching"
	VolumeStatusInUse     = "in_use"
	VolumeStatusDetaching = "detaching"
	VolumeStatusDeleting  = "deleting"
	VolumeStatusDeleted   = "deleted"
	VolumeStatusError     = "error"
)

// ── VolumeAttachmentRow ───────────────────────────────────────────────────────

// VolumeAttachmentRow is the DB representation of a volume attachment record.
// Source: P2_VOLUME_MODEL.md §6, §4.
type VolumeAttachmentRow struct {
	ID                  string
	VolumeID            string
	InstanceID          string
	DevicePath          string
	DeleteOnTermination bool
	AttachedAt          time.Time
	DetachedAt          *time.Time // NULL while active
}

// ── Volume CRUD ───────────────────────────────────────────────────────────────

// CreateVolume inserts a new volume record in 'creating' status.
// Source: P2_VOLUME_MODEL.md §3.2 (creating is the initial state).
func (r *Repo) CreateVolume(ctx context.Context, row *VolumeRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO volumes (
			id, owner_principal_id, display_name, region, availability_zone,
			size_gb, origin, source_disk_id, source_snapshot_id,
			status, storage_pool_id,
			version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'creating',$10,0,NOW(),NOW())
	`,
		row.ID, row.OwnerPrincipalID, row.DisplayName, row.Region, row.AvailabilityZone,
		row.SizeGB, row.Origin, row.SourceDiskID, row.SourceSnapshotID,
		row.StoragePoolID,
	)
	if err != nil {
		return fmt.Errorf("CreateVolume: %w", err)
	}
	return nil
}

// GetVolumeByID fetches a single volume by its primary key.
// Returns nil, nil when no matching row exists.
// Does NOT filter by owner — caller must enforce ownership.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (caller enforces 404-on-mismatch).
func (r *Repo) GetVolumeByID(ctx context.Context, id string) (*VolumeRow, error) {
	row := &VolumeRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_principal_id, display_name, region, availability_zone,
		       size_gb, origin, source_disk_id, source_snapshot_id,
		       status, storage_path, storage_pool_id,
		       version, locked_by, created_at, updated_at, deleted_at
		FROM volumes
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id).Scan(
		&row.ID, &row.OwnerPrincipalID, &row.DisplayName, &row.Region, &row.AvailabilityZone,
		&row.SizeGB, &row.Origin, &row.SourceDiskID, &row.SourceSnapshotID,
		&row.Status, &row.StoragePath, &row.StoragePoolID,
		&row.Version, &row.LockedBy, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetVolumeByID: %w", err)
	}
	return row, nil
}

// ListVolumesByOwner returns all non-deleted volumes for a principal, newest first.
// Source: AUTH_OWNERSHIP_MODEL_V1 §4.
func (r *Repo) ListVolumesByOwner(ctx context.Context, ownerPrincipalID string) ([]*VolumeRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_principal_id, display_name, region, availability_zone,
		       size_gb, origin, source_disk_id, source_snapshot_id,
		       status, storage_path, storage_pool_id,
		       version, locked_by, created_at, updated_at, deleted_at
		FROM volumes
		WHERE owner_principal_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, ownerPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("ListVolumesByOwner: %w", err)
	}
	defer rows.Close()

	var out []*VolumeRow
	for rows.Next() {
		row := &VolumeRow{}
		if err := rows.Scan(
			&row.ID, &row.OwnerPrincipalID, &row.DisplayName, &row.Region, &row.AvailabilityZone,
			&row.SizeGB, &row.Origin, &row.SourceDiskID, &row.SourceSnapshotID,
			&row.Status, &row.StoragePath, &row.StoragePoolID,
			&row.Version, &row.LockedBy, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListVolumesByOwner scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateVolumeStatus transitions a volume to a new status using optimistic locking.
// Returns an error if 0 rows were updated (concurrent modification or stale version).
// Source: P2_VOLUME_MODEL.md §3 (state machine), core-architecture-blueprint §optimistic locking.
func (r *Repo) UpdateVolumeStatus(ctx context.Context, id, expectedStatus, newStatus string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volumes
		SET status     = $3,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id      = $1
		  AND status  = $2
		  AND version = $4
		  AND deleted_at IS NULL
	`, id, expectedStatus, newStatus, version)
	if err != nil {
		return fmt.Errorf("UpdateVolumeStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateVolumeStatus: concurrent modification or status mismatch for volume %s", id)
	}
	return nil
}

// LockVolume acquires the mutation lock on a volume by setting locked_by = jobID.
// Uses optimistic locking: only succeeds if version matches and locked_by IS NULL.
// Source: P2_VOLUME_MODEL.md §7 VOL-I-5.
func (r *Repo) LockVolume(ctx context.Context, id, jobID, expectedStatus string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volumes
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
		return fmt.Errorf("LockVolume: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("LockVolume: volume %s is locked or version mismatch", id)
	}
	return nil
}

// UnlockVolume releases the mutation lock and transitions status in a single update.
// Used by workers on completion or failure. Clears locked_by unconditionally.
// Source: P2_VOLUME_MODEL.md §7 VOL-I-5.
func (r *Repo) UnlockVolume(ctx context.Context, id, newStatus string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volumes
		SET locked_by  = NULL,
		    status     = $2,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id, newStatus)
	if err != nil {
		return fmt.Errorf("UnlockVolume: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UnlockVolume: volume %s not found", id)
	}
	return nil
}

// SetVolumeStoragePath persists the storage_path on a volume row.
// Called by VOLUME_CREATE and VOLUME_RESTORE workers after the storage
// data-plane has provisioned the block device or CoW overlay.
// Does not change status or locked_by — those are managed separately.
// Source: P2_VOLUME_MODEL.md §5 (storage_path assigned on create completion).
// VM-P2B-S3.
func (r *Repo) SetVolumeStoragePath(ctx context.Context, id, storagePath string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volumes
		SET storage_path = $2,
		    updated_at   = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id, storagePath)
	if err != nil {
		return fmt.Errorf("SetVolumeStoragePath: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SetVolumeStoragePath: volume %s not found", id)
	}
	return nil
}

// SoftDeleteVolume marks a volume as deleted (sets deleted_at, status=deleted).
// Called after the VOLUME_DELETE job completes successfully.
// Source: P2_VOLUME_MODEL.md §5.2.
func (r *Repo) SoftDeleteVolume(ctx context.Context, id string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volumes
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
		return fmt.Errorf("SoftDeleteVolume: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteVolume: concurrent modification for volume %s", id)
	}
	return nil
}

// HasActiveVolumeJob returns true when the volume already has a pending or
// in_progress job of the given type.
// Source: JOB_MODEL_V1 §idempotency, P2_VOLUME_MODEL.md §7 VOL-I-5.
func (r *Repo) HasActiveVolumeJob(ctx context.Context, volumeID, jobType string) (bool, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM jobs
		WHERE volume_id = $1
		  AND job_type  = $2
		  AND status IN ('pending', 'in_progress')
	`, volumeID, jobType).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("HasActiveVolumeJob: %w", err)
	}
	return count > 0, nil
}

// InsertVolumeJob inserts a job scoped to a volume (no instance_id).
// Volume jobs set volume_id and leave instance_id NULL.
// Source: P2_VOLUME_MODEL.md §4.2 (VOLUME_ATTACH), §4.4 (VOLUME_DETACH), §5.2 (VOLUME_DELETE).
// Note: ON CONFLICT on idempotency_key does nothing — caller checks for existing job.
func (r *Repo) InsertVolumeJob(ctx context.Context, row *JobRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, volume_id, job_type, status,
			idempotency_key, attempt_count, max_attempts,
			created_at, updated_at
		) VALUES ($1,$2,$3,'pending',$4,0,$5,NOW(),NOW())
		ON CONFLICT (idempotency_key) DO NOTHING
	`,
		row.ID, row.VolumeID, row.JobType,
		row.IdempotencyKey, row.MaxAttempts,
	)
	if err != nil {
		return fmt.Errorf("InsertVolumeJob: %w", err)
	}
	return nil
}

// ── Volume Attachment CRUD ────────────────────────────────────────────────────

// CreateVolumeAttachment inserts a new volume attachment record.
// The unique partial index on (volume_id) WHERE detached_at IS NULL enforces VOL-I-1
// at the DB layer — if a concurrent attach races, the INSERT will fail with a
// unique constraint violation which the caller should surface as a 409.
// Source: P2_VOLUME_MODEL.md §4, §7 VOL-I-1.
func (r *Repo) CreateVolumeAttachment(ctx context.Context, row *VolumeAttachmentRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO volume_attachments (
			id, volume_id, instance_id, device_path,
			delete_on_termination, attached_at
		) VALUES ($1,$2,$3,$4,$5,NOW())
	`,
		row.ID, row.VolumeID, row.InstanceID, row.DevicePath,
		row.DeleteOnTermination,
	)
	if err != nil {
		return fmt.Errorf("CreateVolumeAttachment: %w", err)
	}
	return nil
}

// GetActiveAttachmentByVolume returns the active (non-detached) attachment for a volume.
// Returns nil, nil when the volume is not currently attached.
// Source: P2_VOLUME_MODEL.md §4, VOL-I-1.
func (r *Repo) GetActiveAttachmentByVolume(ctx context.Context, volumeID string) (*VolumeAttachmentRow, error) {
	row := &VolumeAttachmentRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, volume_id, instance_id, device_path,
		       delete_on_termination, attached_at, detached_at
		FROM volume_attachments
		WHERE volume_id   = $1
		  AND detached_at IS NULL
	`, volumeID).Scan(
		&row.ID, &row.VolumeID, &row.InstanceID, &row.DevicePath,
		&row.DeleteOnTermination, &row.AttachedAt, &row.DetachedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetActiveAttachmentByVolume: %w", err)
	}
	return row, nil
}

// ListActiveAttachmentsByInstance returns all active (non-detached) volume attachments
// for an instance. Used by GET /v1/instances/{id}/volumes.
// Source: P2_VOLUME_MODEL.md §8.
func (r *Repo) ListActiveAttachmentsByInstance(ctx context.Context, instanceID string) ([]*VolumeAttachmentRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, volume_id, instance_id, device_path,
		       delete_on_termination, attached_at, detached_at
		FROM volume_attachments
		WHERE instance_id = $1
		  AND detached_at IS NULL
		ORDER BY attached_at ASC
	`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("ListActiveAttachmentsByInstance: %w", err)
	}
	defer rows.Close()

	var out []*VolumeAttachmentRow
	for rows.Next() {
		row := &VolumeAttachmentRow{}
		if err := rows.Scan(
			&row.ID, &row.VolumeID, &row.InstanceID, &row.DevicePath,
			&row.DeleteOnTermination, &row.AttachedAt, &row.DetachedAt,
		); err != nil {
			return nil, fmt.Errorf("ListActiveAttachmentsByInstance scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CloseVolumeAttachment sets detached_at on the active attachment record.
// Called by the VOLUME_DETACH worker on successful detach.
// Source: P2_VOLUME_MODEL.md §4.4 (detach flow step 4).
func (r *Repo) CloseVolumeAttachment(ctx context.Context, attachmentID string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE volume_attachments
		SET detached_at = NOW()
		WHERE id          = $1
		  AND detached_at IS NULL
	`, attachmentID)
	if err != nil {
		return fmt.Errorf("CloseVolumeAttachment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("CloseVolumeAttachment: attachment %s not found or already closed", attachmentID)
	}
	return nil
}

// CountActiveAttachmentsByInstance returns the number of volumes currently attached
// to an instance. Used to enforce the maximum volumes per instance limit.
// Source: P2_VOLUME_MODEL.md §4.1 (maximum 16 volumes per instance).
func (r *Repo) CountActiveAttachmentsByInstance(ctx context.Context, instanceID string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM volume_attachments
		WHERE instance_id = $1
		  AND detached_at IS NULL
	`, instanceID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountActiveAttachmentsByInstance: %w", err)
	}
	return count, nil
}

// NextDevicePath returns the next available device path for an instance.
// Assigns /dev/vdb, /dev/vdc, ... based on existing active attachments.
// /dev/vda is reserved for the root disk.
// Source: P2_VOLUME_MODEL.md §4.1 (device path assignment).
func (r *Repo) NextDevicePath(ctx context.Context, instanceID string) (string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT device_path
		FROM volume_attachments
		WHERE instance_id = $1
		  AND detached_at IS NULL
		ORDER BY device_path ASC
	`, instanceID)
	if err != nil {
		return "", fmt.Errorf("NextDevicePath: %w", err)
	}
	defer rows.Close()

	used := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return "", fmt.Errorf("NextDevicePath scan: %w", err)
		}
		used[path] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	// Assign the next slot: /dev/vdb through /dev/vdp (15 data volumes, vda is root).
	for i := 1; i < 16; i++ {
		// 'b' is rune 98; 'b'+0 = vdb, 'b'+1 = vdc, ...
		path := "/dev/vd" + string(rune('b'+i-1))
		if !used[path] {
			return path, nil
		}
	}
	return "", fmt.Errorf("NextDevicePath: no available device paths for instance %s", instanceID)
}

// ── VM Job 5: Reconciliation scan methods ─────────────────────────────────────

// VolumeOrphanRow is the result row for ListVolumesWithOrphanStorage.
// Captures volumes that have a storage_path but are in a terminal/cleaned state
// where the physical storage may need GC.
type VolumeOrphanRow struct {
	VolumeID    string
	StoragePath string
	Status      string
	Version     int
}

// ListVolumesWithOrphanStorage returns volumes that have storage_path set but
// whose status indicates the volume record has been deleted or is in error.
// The reconciling sub-scan uses this to detect orphan storage artifacts that
// may be safe to clean up.
// VM Job 5 — Case 6: Volume artifact exists but DB says deleted.
func (r *Repo) ListVolumesWithOrphanStorage(ctx context.Context) ([]*VolumeOrphanRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, COALESCE(storage_path,''), status, version
		FROM volumes
		WHERE storage_path IS NOT NULL
		  AND storage_path != ''
		  AND status IN ('deleted', 'error')
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("ListVolumesWithOrphanStorage: %w", err)
	}
	defer rows.Close()

	var out []*VolumeOrphanRow
	for rows.Next() {
		r := &VolumeOrphanRow{}
		if err := rows.Scan(&r.VolumeID, &r.StoragePath, &r.Status, &r.Version); err != nil {
			return nil, fmt.Errorf("ListVolumesWithOrphanStorage scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StaleAttachmentRow is the result row for ListStaleAttachments.
// Captures attachments whose instance or volume is in a terminal state.
type StaleAttachmentRow struct {
	AttachmentID  string
	VolumeID      string
	InstanceID    string
	InstanceState string
	VolumeState   string
}

// ListStaleAttachments returns active volume attachments where the owning
// instance or volume is in a terminal state (deleted, failed, error).
// VM Job 5 — Case 7: DB attachment intent exists but runtime disk attachment missing.
func (r *Repo) ListStaleAttachments(ctx context.Context) ([]*StaleAttachmentRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			a.id          AS attachment_id,
			a.volume_id,
			a.instance_id,
			COALESCE(i.vm_state, 'unknown') AS instance_state,
			COALESCE(v.status,   'unknown') AS volume_state
		FROM volume_attachments a
		LEFT JOIN instances i ON i.id = a.instance_id
		LEFT JOIN volumes   v ON v.id = a.volume_id
		WHERE a.detached_at IS NULL
		  AND (
		      i.deleted_at IS NOT NULL
		      OR i.vm_state IN ('deleted', 'failed')
		      OR v.deleted_at IS NOT NULL
		      OR v.status IN ('deleted', 'error')
		  )
		ORDER BY a.attached_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("ListStaleAttachments: %w", err)
	}
	defer rows.Close()

	var out []*StaleAttachmentRow
	for rows.Next() {
		r := &StaleAttachmentRow{}
		if err := rows.Scan(&r.AttachmentID, &r.VolumeID, &r.InstanceID,
			&r.InstanceState, &r.VolumeState,
		); err != nil {
			return nil, fmt.Errorf("ListStaleAttachments scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
