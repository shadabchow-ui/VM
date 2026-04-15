package main

// volume_types.go — Public API request/response DTOs for volume resources.
//
// VM-P2B Slice 1: first-class independent block volume API surface.
// VM-P2B-S3: Added SourceSnapshotID to VolumeResponse so volumes with
//            origin=snapshot expose their source reference to callers.
//
// Follows the same conventions as instance_types.go:
//   - Request structs use json tags with omitempty on optional fields.
//   - Response structs expose only the fields appropriate for external clients.
//   - Async operations return a VolumeJobResponse (202) with job_id.
//
// Source: P2_VOLUME_MODEL.md §2.3 (resource shape), §8 (API endpoints),
//         API_ERROR_CONTRACT_V1 §1 (envelope shape),
//         AUTH_OWNERSHIP_MODEL_V1 §3 (ownership).

import "time"

// ── Volume resource ───────────────────────────────────────────────────────────

// VolumeResponse is the canonical JSON shape returned by volume endpoints.
// Source: P2_VOLUME_MODEL.md §2.3.
type VolumeResponse struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	Region           string                    `json:"region"`
	AvailabilityZone string                    `json:"availability_zone"`
	SizeGB           int                       `json:"size_gb"`
	Status           string                    `json:"status"`
	Origin           string                    `json:"origin"`
	SourceDiskID     *string                   `json:"source_disk_id,omitempty"`
	SourceSnapshotID *string                   `json:"source_snapshot_id,omitempty"` // VM-P2B-S3: set for origin=snapshot
	Attachment       *VolumeAttachmentResponse `json:"attachment,omitempty"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

// VolumeAttachmentResponse is the inline attachment object in VolumeResponse.
// Source: P2_VOLUME_MODEL.md §2.3 (attachment object).
type VolumeAttachmentResponse struct {
	InstanceID          string    `json:"instance_id"`
	DevicePath          string    `json:"device_path"`
	DeleteOnTermination bool      `json:"delete_on_termination"`
	AttachedAt          time.Time `json:"attached_at"`
}

// ── Create ────────────────────────────────────────────────────────────────────

// CreateVolumeRequest is the payload for POST /v1/volumes.
// Source: P2_VOLUME_MODEL.md §2 (blank volume creation), §8.
type CreateVolumeRequest struct {
	Name             string `json:"name"`
	AvailabilityZone string `json:"availability_zone"`
	SizeGB           int    `json:"size_gb"`
}

// CreateVolumeResponse is returned from POST /v1/volumes with 202 Accepted.
// Source: P2_VOLUME_MODEL.md §8, JOB_MODEL_V1 §1.
type CreateVolumeResponse struct {
	Volume VolumeResponse `json:"volume"`
	JobID  string         `json:"job_id"`
}

// ── List ──────────────────────────────────────────────────────────────────────

// ListVolumesResponse wraps the volume list.
// Source: P2_VOLUME_MODEL.md §8.
type ListVolumesResponse struct {
	Volumes []VolumeResponse `json:"volumes"`
	Total   int              `json:"total"`
}

// ── Delete ────────────────────────────────────────────────────────────────────

// VolumeLifecycleResponse is returned by delete, attach, and detach endpoints.
// Contains the enqueued job_id so the caller can poll for completion.
// Mirrors LifecycleResponse for instances — same contract shape.
// Source: JOB_MODEL_V1 §1, P2_VOLUME_MODEL.md §4.2, §4.4, §5.2.
type VolumeLifecycleResponse struct {
	VolumeID string `json:"volume_id"`
	JobID    string `json:"job_id"`
	Action   string `json:"action"`
}

// ── Attach ────────────────────────────────────────────────────────────────────

// AttachVolumeRequest is the payload for POST /v1/instances/{id}/volumes.
// Source: P2_VOLUME_MODEL.md §4.2 (attach flow step 1).
type AttachVolumeRequest struct {
	VolumeID            string  `json:"volume_id"`
	DevicePath          *string `json:"device_path,omitempty"`           // system-assigned if absent
	DeleteOnTermination *bool   `json:"delete_on_termination,omitempty"` // defaults to false for data volumes
}

// ── Instance volumes list ─────────────────────────────────────────────────────

// InstanceVolumeEntry is the per-attachment shape in GET /v1/instances/{id}/volumes.
// Combines volume identity with attachment metadata.
// Source: P2_VOLUME_MODEL.md §8.
type InstanceVolumeEntry struct {
	VolumeID            string    `json:"volume_id"`
	Name                string    `json:"name"`
	SizeGB              int       `json:"size_gb"`
	Status              string    `json:"status"`
	DevicePath          string    `json:"device_path"`
	DeleteOnTermination bool      `json:"delete_on_termination"`
	AttachedAt          time.Time `json:"attached_at"`
}

// ListInstanceVolumesResponse wraps the per-instance volume list.
// Source: P2_VOLUME_MODEL.md §8.
type ListInstanceVolumesResponse struct {
	Volumes []InstanceVolumeEntry `json:"volumes"`
	Total   int                   `json:"total"`
}
