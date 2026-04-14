package domainmodel

import "time"

// snapshot.go — Snapshot domain model.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (identity, state machine, resource shape),
//         vm-15-02__skill__snapshot-clone-restore-retention-model.md.
//
// VM-P2B-S2: first-class snapshot resource.
//
// Design: snapshots are decoupled from their source volume. Deleting the source
// volume does not delete the snapshot (Independent Resource Lifecycle — blueprint
// core contract). Snapshots are point-in-time read-only artifacts. A volume can
// be created from a snapshot (restore/clone) via VOLUME_RESTORE job.

// ── Snapshot Status ───────────────────────────────────────────────────────────

// SnapshotStatus is the canonical snapshot state enum.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.4.
type SnapshotStatus string

const (
	SnapshotStatusPending   SnapshotStatus = "pending"
	SnapshotStatusCreating  SnapshotStatus = "creating"
	SnapshotStatusAvailable SnapshotStatus = "available"
	SnapshotStatusError     SnapshotStatus = "error"
	SnapshotStatusDeleting  SnapshotStatus = "deleting"
	SnapshotStatusDeleted   SnapshotStatus = "deleted"
)

// SnapshotTransitionalStatuses are states that block new state-mutating operations.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5.
var SnapshotTransitionalStatuses = map[SnapshotStatus]bool{
	SnapshotStatusPending:  true,
	SnapshotStatusCreating: true,
	SnapshotStatusDeleting: true,
}

// ── Snapshot ──────────────────────────────────────────────────────────────────

// Snapshot is the canonical domain object for a point-in-time storage snapshot.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.3.
type Snapshot struct {
	ID               string         `db:"id"`
	OwnerPrincipalID string         `db:"owner_principal_id"`
	DisplayName      string         `db:"display_name"`
	Region           string         `db:"region"`
	SourceVolumeID   *string        `db:"source_volume_id"`   // nullable; source may be deleted
	SourceInstanceID *string        `db:"source_instance_id"` // nullable; set for root-disk snapshots
	SizeGB           int            `db:"size_gb"`
	Status           SnapshotStatus `db:"status"`
	ProgressPercent  int            `db:"progress_percent"`
	StoragePath      *string        `db:"storage_path"`
	StoragePoolID    *string        `db:"storage_pool_id"`
	Encrypted        bool           `db:"encrypted"`
	Version          int            `db:"version"`
	LockedBy         *string        `db:"locked_by"` // job_id holding mutation lock
	CreatedAt        time.Time      `db:"created_at"`
	CompletedAt      *time.Time     `db:"completed_at"` // set when status → available
	UpdatedAt        time.Time      `db:"updated_at"`
	DeletedAt        *time.Time     `db:"deleted_at"`
}

// ── Snapshot Job Types ────────────────────────────────────────────────────────

// Snapshot job type constants.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (API endpoints — all mutating ops are async).
const (
	JobTypeSnapshotCreate JobType = "SNAPSHOT_CREATE"
	JobTypeSnapshotDelete JobType = "SNAPSHOT_DELETE"

	// JobTypeVolumeRestore creates a new volume from a snapshot.
	// Extends the volume job type set. Stored in jobs.snapshot_id (not volume_id)
	// until the new volume is created; the new volume_id is recorded on completion.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (restore flow).
	JobTypeVolumeRestore JobType = "VOLUME_RESTORE"
)
