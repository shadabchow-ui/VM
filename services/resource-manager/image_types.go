package main

// image_types.go — Public API request/response DTOs for image resources.
//
// VM-P2C-P1: read-only image list/get for this slice.
//
// Follows the same conventions as snapshot_types.go:
//   - Response structs expose only fields appropriate for external clients.
//   - storage_url and owner_id are internal; not exposed in API responses.
//   - nullable timestamps use *time.Time with omitempty.
//
// Source: INSTANCE_MODEL_V1.md §7 (Phase 1 image model),
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.3 (custom image fields),
//         API_ERROR_CONTRACT_V1.md §1 (envelope shape),
//         08-01-api-resource-model-and-endpoint-design.md §images.

import "time"

// ── Image resource ────────────────────────────────────────────────────────────

// ImageResponse is the canonical JSON shape returned by image endpoints.
//
// Excluded intentionally:
//   - storage_url: internal infrastructure detail, never exposed to callers.
//   - owner_id:    internal principal UUID; not appropriate for external response.
//   - validation_status: internal pipeline detail; not surfaced in this slice.
//   - source_snapshot_id: Phase 2 field; omitted until custom-image-create slice.
//
// Source: INSTANCE_MODEL_V1.md §7, P2_IMAGE_SNAPSHOT_MODEL.md §3.3.
type ImageResponse struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	OSFamily     string     `json:"os_family"`
	OSVersion    string     `json:"os_version"`
	Architecture string     `json:"architecture"`
	Visibility   string     `json:"visibility"`
	SourceType   string     `json:"source_type"`
	MinDiskGB    int        `json:"min_disk_gb"`
	Status       string     `json:"status"`
	DeprecatedAt *time.Time `json:"deprecated_at,omitempty"`
	ObsoletedAt  *time.Time `json:"obsoleted_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
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
