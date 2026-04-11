package db

// root_disk_repo.go — Root disk resource persistence methods.
//
// Source: INSTANCE_MODEL_V1.md §8, 06-01-root-disk-model-and-persistence-semantics.md,
//         core-architecture-blueprint.md (root_disks schema), P2_VOLUME_MODEL.md §1.
// M10 Slice 2: Volume Foundation — introduces RootDiskRow and CRUD operations.
//
// A root disk is a first-class internal object stored independently from the instance.
// This separation enables Phase 2 persistent volumes where disks can outlive instances.
//
// Status values:
//   - CREATING: CoW overlay being materialized
//   - ATTACHED: Bound to a running or stopped instance
//   - DETACHED: Instance deleted with delete_on_termination=false (Phase 2 volume entry point)

import (
	"context"
	"fmt"
	"time"
)

// ── Root Disk Status Constants ──────────────────────────────────────────────

// RootDiskStatus values per 06-01-root-disk-model-and-persistence-semantics.md
const (
	RootDiskStatusCreating = "CREATING"
	RootDiskStatusAttached = "ATTACHED"
	RootDiskStatusDetached = "DETACHED"
)

// ── RootDiskRow ─────────────────────────────────────────────────────────────

// RootDiskRow is the DB representation of a root disk record.
// Source: INSTANCE_MODEL_V1.md §8, core-architecture-blueprint.md.
type RootDiskRow struct {
	DiskID              string     // UUID primary key
	InstanceID          *string    // FK to instances, nullable (NULL when detached)
	SourceImageID       string     // UUID of the base image
	StoragePoolID       string     // UUID of the storage pool
	StoragePath         string     // e.g., nfs://filer/vol/disk_id.qcow2
	SizeGB              int        // Disk size in GB
	DeleteOnTermination bool       // If true, disk is deleted with instance
	Status              string     // CREATING, ATTACHED, DETACHED
	CreatedAt           time.Time
}

// ── Root Disk CRUD ──────────────────────────────────────────────────────────

// CreateRootDisk inserts a new root disk record.
// Called during instance creation after storage allocation.
// Source: 06-01-root-disk-model-and-persistence-semantics.md (schema).
func (r *Repo) CreateRootDisk(ctx context.Context, row *RootDiskRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO root_disks (
			disk_id, instance_id, source_image_id, storage_pool_id,
			storage_path, size_gb, delete_on_termination, status, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
	`,
		row.DiskID, row.InstanceID, row.SourceImageID, row.StoragePoolID,
		row.StoragePath, row.SizeGB, row.DeleteOnTermination, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateRootDisk: %w", err)
	}
	return nil
}

// GetRootDiskByID fetches a root disk by its primary key.
// Returns nil, nil when no matching row exists.
func (r *Repo) GetRootDiskByID(ctx context.Context, diskID string) (*RootDiskRow, error) {
	row := &RootDiskRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT disk_id, instance_id, source_image_id, storage_pool_id,
		       storage_path, size_gb, delete_on_termination, status, created_at
		FROM root_disks
		WHERE disk_id = $1
	`, diskID).Scan(
		&row.DiskID, &row.InstanceID, &row.SourceImageID, &row.StoragePoolID,
		&row.StoragePath, &row.SizeGB, &row.DeleteOnTermination, &row.Status,
		&row.CreatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetRootDiskByID: %w", err)
	}
	return row, nil
}

// GetRootDiskByInstanceID fetches the root disk attached to an instance.
// Returns nil, nil when no matching row exists.
// An instance has exactly one root disk while running.
func (r *Repo) GetRootDiskByInstanceID(ctx context.Context, instanceID string) (*RootDiskRow, error) {
	row := &RootDiskRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT disk_id, instance_id, source_image_id, storage_pool_id,
		       storage_path, size_gb, delete_on_termination, status, created_at
		FROM root_disks
		WHERE instance_id = $1
	`, instanceID).Scan(
		&row.DiskID, &row.InstanceID, &row.SourceImageID, &row.StoragePoolID,
		&row.StoragePath, &row.SizeGB, &row.DeleteOnTermination, &row.Status,
		&row.CreatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetRootDiskByInstanceID: %w", err)
	}
	return row, nil
}

// UpdateRootDiskStatus updates the status of a root disk.
// Used by workers during lifecycle transitions.
// Returns error if disk not found (0 rows affected).
func (r *Repo) UpdateRootDiskStatus(ctx context.Context, diskID, status string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE root_disks
		SET status = $2
		WHERE disk_id = $1
	`, diskID, status)
	if err != nil {
		return fmt.Errorf("UpdateRootDiskStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateRootDiskStatus: disk %s not found", diskID)
	}
	return nil
}

// DetachRootDisk detaches a root disk from its instance.
// Sets instance_id = NULL and status = DETACHED.
// Called when an instance is deleted with delete_on_termination=false.
// Source: 06-01-root-disk-model-and-persistence-semantics.md (delete semantics).
func (r *Repo) DetachRootDisk(ctx context.Context, diskID string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE root_disks
		SET instance_id = NULL,
		    status = $2
		WHERE disk_id = $1
	`, diskID, RootDiskStatusDetached)
	if err != nil {
		return fmt.Errorf("DetachRootDisk: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("DetachRootDisk: disk %s not found", diskID)
	}
	return nil
}

// DeleteRootDisk removes a root disk record.
// Called when an instance is deleted with delete_on_termination=true.
// The physical storage deallocation is handled by the worker; this is DB cleanup.
func (r *Repo) DeleteRootDisk(ctx context.Context, diskID string) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM root_disks
		WHERE disk_id = $1
	`, diskID)
	if err != nil {
		return fmt.Errorf("DeleteRootDisk: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("DeleteRootDisk: disk %s not found", diskID)
	}
	return nil
}

// ListDetachedRootDisks returns all root disks with status=DETACHED.
// Used by Phase 2 volume service to surface detached disks as volumes.
// Source: P2_VOLUME_MODEL.md §1 (DETACHED root disk is seed for volume service).
func (r *Repo) ListDetachedRootDisks(ctx context.Context, limit int) ([]*RootDiskRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT disk_id, instance_id, source_image_id, storage_pool_id,
		       storage_path, size_gb, delete_on_termination, status, created_at
		FROM root_disks
		WHERE status = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, RootDiskStatusDetached, limit)
	if err != nil {
		return nil, fmt.Errorf("ListDetachedRootDisks: %w", err)
	}
	defer rows.Close()

	var out []*RootDiskRow
	for rows.Next() {
		row := &RootDiskRow{}
		if err := rows.Scan(
			&row.DiskID, &row.InstanceID, &row.SourceImageID, &row.StoragePoolID,
			&row.StoragePath, &row.SizeGB, &row.DeleteOnTermination, &row.Status,
			&row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListDetachedRootDisks scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
