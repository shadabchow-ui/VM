package main

// snapshot_types.go — Public API request/response DTOs for snapshot resources.
//
// VM-P2B-S2: snapshot create/list/describe/delete + restore-to-volume.
// VM-P2B-S3: RestoreSnapshotResponse now embeds the full VolumeResponse
//            instead of only volume_id, matching the contract in
//            P2_IMAGE_SNAPSHOT_MODEL.md §4.
//
// Follows the same conventions as volume_types.go:
//   - Request structs use json tags with omitempty on optional fields.
//   - Response structs expose only the fields appropriate for external clients.
//   - Async operations return 202 with a job_id.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.3 (resource shape), §4 (API endpoints),
//         API_ERROR_CONTRACT_V1 §1 (envelope shape),
//         AUTH_OWNERSHIP_MODEL_V1 §3 (ownership).

import "time"

// ── Snapshot resource ─────────────────────────────────────────────────────────

// SnapshotResponse is the canonical JSON shape returned by snapshot endpoints.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.3.
type SnapshotResponse struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Region           string     `json:"region"`
	SourceVolumeID   *string    `json:"source_volume_id,omitempty"`
	SourceInstanceID *string    `json:"source_instance_id,omitempty"`
	SizeGB           int        `json:"size_gb"`
	Status           string     `json:"status"`
	ProgressPercent  int        `json:"progress_percent"`
	Encrypted        bool       `json:"encrypted"`
	CreatedAt        time.Time  `json:"created_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ── Create ────────────────────────────────────────────────────────────────────

// CreateSnapshotRequest is the payload for POST /v1/snapshots.
// Exactly one of source_volume_id or source_instance_id must be provided.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6 (creation rules), §4.
type CreateSnapshotRequest struct {
	Name             string  `json:"name"`
	SourceVolumeID   *string `json:"source_volume_id,omitempty"`
	SourceInstanceID *string `json:"source_instance_id,omitempty"`
}

// CreateSnapshotResponse is returned from POST /v1/snapshots with 202 Accepted.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4, JOB_MODEL_V1 §1.
type CreateSnapshotResponse struct {
	Snapshot SnapshotResponse `json:"snapshot"`
	JobID    string           `json:"job_id"`
}

// ── List ──────────────────────────────────────────────────────────────────────

// ListSnapshotsResponse wraps the snapshot list.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4.
type ListSnapshotsResponse struct {
	Snapshots []SnapshotResponse `json:"snapshots"`
	Total     int                `json:"total"`
}

// ── Lifecycle (delete) ────────────────────────────────────────────────────────

// SnapshotLifecycleResponse is returned by delete.
// Contains the enqueued job_id so the caller can poll for completion.
// Source: JOB_MODEL_V1 §1, P2_IMAGE_SNAPSHOT_MODEL.md §4.
type SnapshotLifecycleResponse struct {
	SnapshotID string `json:"snapshot_id"`
	JobID      string `json:"job_id"`
	Action     string `json:"action"`
}

// ── Restore (create volume from snapshot) ─────────────────────────────────────

// RestoreSnapshotRequest is the payload for POST /v1/snapshots/{id}/restore.
// Creates a new volume whose content is restored from this snapshot.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (restore flow),
//
//	vm-15-02__blueprint__ §interaction_or_ops_contract.
type RestoreSnapshotRequest struct {
	Name             string `json:"name"`
	AvailabilityZone string `json:"availability_zone"`
	// size_gb is optional override; defaults to snapshot size_gb.
	// Must be >= snapshot size_gb.
	SizeGB *int `json:"size_gb,omitempty"`
}

// RestoreSnapshotResponse is returned from POST /v1/snapshots/{id}/restore with 202.
// The new volume starts in 'creating' status and is advanced by the VOLUME_RESTORE worker.
//
// Returns the full volume resource so callers do not need a separate GET to inspect
// the new volume's identity, AZ, and size.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (restore → new volume), JOB_MODEL_V1 §1.
// VM-P2B-S3: changed from {volume_id, job_id} to {volume, job_id}.
type RestoreSnapshotResponse struct {
	Volume VolumeResponse `json:"volume"`
	JobID  string         `json:"job_id"`
}
