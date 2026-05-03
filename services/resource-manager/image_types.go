package main

// image_types.go — Public API request/response DTOs for image resources.
//
// VM-P2C-P1: read-only image list/get (unchanged).
// VM-P2C-P2: added:
//   - CreateImageFromSnapshotRequest / CreateImageFromSnapshotResponse
//   - ImportImageRequest / ImportImageResponse
//   - DeprecateImageResponse / ObsoleteImageResponse
//   - source_snapshot_id exposed in ImageResponse (custom image field)
//
// VM-P2C-P3: added:
//   - FamilyName / FamilyVersion exposed in ImageResponse when set.
//   - family_name / family_version optional fields on CreateImageFromSnapshotRequest
//     and ImportImageRequest so images can be assigned to a family on creation.
//   - ImageFamilyRef embedded in CreateInstanceRequest for family-alias launch.
//
// Follows the same conventions as snapshot_types.go:
//   - Response structs expose only fields appropriate for external clients.
//   - storage_url and owner_id are internal; not exposed in API responses.
//   - nullable timestamps use *time.Time with omitempty.
//   - Async operations return 202 with a job_id.
//
// Source: INSTANCE_MODEL_V1.md §7 (Phase 1 image model),
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.3 (custom image fields),
//         API_ERROR_CONTRACT_V1.md §1 (envelope shape),
//         08-01-api-resource-model-and-endpoint-design.md §images,
//         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md §family_seam.

import "time"

// ── Image resource ────────────────────────────────────────────────────────────

// ImageResponse is the canonical JSON shape returned by image endpoints.
//
// Excluded intentionally:
//   - storage_url: internal infrastructure detail, never exposed to callers.
//   - owner_id:    internal principal UUID; not appropriate for external response.
//   - validation_status: internal pipeline detail; not surfaced in the API.
//   - import_url: internal pipeline detail; not surfaced in the API.
//
// Included as of VM-P2C-P3:
//   - family_name / family_version: omitted when nil (images not in a family).
//
// Source: INSTANCE_MODEL_V1.md §7, P2_IMAGE_SNAPSHOT_MODEL.md §3.3,
//
//	vm-13-01__blueprint__ §family_seam.
type ImageResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	OSFamily     string `json:"os_family"`
	OSVersion    string `json:"os_version"`
	Architecture string `json:"architecture"`
	Visibility   string `json:"visibility"`
	SourceType   string `json:"source_type"`
	MinDiskGB    int    `json:"min_disk_gb"`
	Status       string `json:"status"`
	// source_snapshot_id is exposed for custom (SNAPSHOT-sourced) images so callers
	// can trace which snapshot the image was created from.
	// nil / omitted for PLATFORM and IMPORT images.
	// VM-P2C-P2.
	SourceSnapshotID *string `json:"source_snapshot_id,omitempty"`
	// family_name / family_version: set when the image belongs to a named family.
	// Omitted (null) for images without family membership.
	// VM-P2C-P3. Source: vm-13-01__blueprint__ §family_seam.
	FamilyName    *string    `json:"family_name,omitempty"`
	FamilyVersion *int       `json:"family_version,omitempty"`
	DeprecatedAt  *time.Time `json:"deprecated_at,omitempty"`
	ObsoletedAt   *time.Time `json:"obsoleted_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ── List ──────────────────────────────────────────────────────────────────────

// ListImagesResponse wraps the image list returned by GET /v1/images.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (GET /v1/images),
//
//	08-01-api-resource-model-and-endpoint-design.md §images.
type ListImagesResponse struct {
	Images []ImageResponse `json:"images"`
	Total  int             `json:"total"`
}

// ── Create from snapshot ──────────────────────────────────────────────────────

// CreateImageFromSnapshotRequest is the payload for POST /v1/images when
// source_type = "SNAPSHOT".
//
// The snapshot must be owned by the caller and in "available" status.
// The resulting image starts in PENDING_VALIDATION status.
//
// VM-P2C-P3: family_name and family_version are optional. When provided the
// image is registered as a member of the named family with the given version.
// family_version requires family_name to be set.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.6 (snapshot → custom image creation flow),
//
//	vm-13-01__blueprint__ §family_seam.
type CreateImageFromSnapshotRequest struct {
	Name             string `json:"name"`
	SourceSnapshotID string `json:"source_snapshot_id"`
	OSFamily         string `json:"os_family"`
	OSVersion        string `json:"os_version"`
	Architecture     string `json:"architecture"`
	// MinDiskGB is optional; defaults to snapshot size_gb when omitted.
	MinDiskGB *int `json:"min_disk_gb,omitempty"`
	// Family membership (optional). VM-P2C-P3.
	FamilyName    *string `json:"family_name,omitempty"`
	FamilyVersion *int    `json:"family_version,omitempty"`
}

// CreateImageFromSnapshotResponse is returned from POST /v1/images (snapshot source)
// with 202 Accepted.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4, JOB_MODEL_V1 §1.
type CreateImageFromSnapshotResponse struct {
	Image ImageResponse `json:"image"`
	JobID string        `json:"job_id"`
}

// ── Import ────────────────────────────────────────────────────────────────────

// ImportImageRequest is the payload for POST /v1/images when
// source_type = "IMPORT".
//
// The caller supplies a storage artifact URL. The control plane creates an image
// resource in PENDING_VALIDATION status and enqueues an IMAGE_IMPORT job.
// The import worker validates the artifact and transitions the image to ACTIVE
// or FAILED.
//
// VM-P2C-P3: family_name and family_version are optional.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (import lifecycle),
//
//	vm-13-01__blueprint__ §family_seam.
type ImportImageRequest struct {
	Name         string `json:"name"`
	ImportURL    string `json:"import_url"`
	OSFamily     string `json:"os_family"`
	OSVersion    string `json:"os_version"`
	Architecture string `json:"architecture"`
	MinDiskGB    int    `json:"min_disk_gb"`
	// Family membership (optional). VM-P2C-P3.
	FamilyName    *string `json:"family_name,omitempty"`
	FamilyVersion *int    `json:"family_version,omitempty"`
}

// ImportImageResponse is returned from POST /v1/images (import source) with 202 Accepted.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4, JOB_MODEL_V1 §1.
type ImportImageResponse struct {
	Image ImageResponse `json:"image"`
	JobID string        `json:"job_id"`
}

// ── Deprecate / Obsolete ──────────────────────────────────────────────────────

// DeprecateImageResponse is returned from POST /v1/images/{id}/deprecate with 200.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
type DeprecateImageResponse struct {
	Image ImageResponse `json:"image"`
}

// ObsoleteImageResponse is returned from POST /v1/images/{id}/obsolete with 200.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
type ObsoleteImageResponse struct {
	Image ImageResponse `json:"image"`
}

// ── Promote ───────────────────────────────────────────────────────────────────

// PromoteImageResponse is returned from POST /v1/images/{id}/promote with 200.
// VM-TRUSTED-IMAGE-FACTORY-PHASE-J: promotion gate response.
type PromoteImageResponse struct {
	Image ImageResponse `json:"image"`
}

// ── Family alias ref (for create-instance) ────────────────────────────────────

// ImageFamilyRef is the optional family-based image selection field embedded in
// CreateInstanceRequest.
//
// When image_family is set, the create-instance handler resolves the family alias
// to a concrete image ID before running admission checks. Exactly one of image_id
// or image_family must be set on any given create-instance request.
//
// FamilyVersion is optional. When nil, the latest launchable image in the family
// is selected (highest family_version, then newest created_at).
// When set, the exact version is required to exist and be launchable.
//
// Source: vm-13-01__blueprint__ §family_seam,
//
//	08-01-api-resource-model-and-endpoint-design.md §CreateInstance.
type ImageFamilyRef struct {
	FamilyName    string `json:"family_name"`
	FamilyVersion *int   `json:"family_version,omitempty"`
}
