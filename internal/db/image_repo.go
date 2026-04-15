package db

// image_repo.go — Image table persistence methods.
//
// VM-P2C-P1: first-class image resource model for admission checks and list/get API.
//
// Source: INSTANCE_MODEL_V1.md §7 (schema, Phase 1 image model),
//         P2_IMAGE_SNAPSHOT_MODEL.md §3 (lifecycle states, visibility),
//         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md
//             §core_contracts "Image Lifecycle State Enforcement",
//         AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-cross-account),
//         API_ERROR_CONTRACT_V1.md §4.
//
// Ownership model:
//   - PUBLIC images (visibility=PUBLIC): accessible to all principals.
//   - PRIVATE images: only accessible to the owning principal (owner_id).
//   - Cross-account reads return nil — caller enforces 404 (not 403).
//   Source: AUTH_OWNERSHIP_MODEL_V1.md §3.
//
// Admission contract:
//   - GetImageForAdmission fetches an image by ID and enforces visibility against
//     the requesting principal. Returns nil if the image does not exist or is not
//     visible to the caller. Status enforcement (OBSOLETE/FAILED blocking) is done
//     by the caller using ImageIsLaunchable(row.Status).
//   Source: vm-13-01__blueprint__ §core_contracts, P2_IMAGE_SNAPSHOT_MODEL.md §3.8.

import (
	"context"
	"fmt"
	"time"
)

// ── ImageRow ──────────────────────────────────────────────────────────────────

// ImageRow is the DB representation of an image record.
// Column order matches the SELECT list used in all image queries below.
// Source: INSTANCE_MODEL_V1.md §7, P2_IMAGE_SNAPSHOT_MODEL.md §3,
//
//	db/migrations/006_images.up.sql.
type ImageRow struct {
	ID               string
	Name             string
	OSFamily         string
	OSVersion        string
	Architecture     string
	OwnerID          string
	Visibility       string // "PUBLIC" | "PRIVATE"
	SourceType       string // "PLATFORM" | "USER" | "SNAPSHOT"
	StorageURL       string
	MinDiskGB        int
	Status           string // "ACTIVE" | "DEPRECATED" | "OBSOLETE" | "FAILED" | "PENDING_VALIDATION"
	ValidationStatus string // "pending" | "validating" | "passed" | "failed"
	DeprecatedAt     *time.Time
	ObsoletedAt      *time.Time
	SourceSnapshotID *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Image status constants — used at the DB layer without importing domain-model.
// Source: vm-13-01__blueprint__ §Image Catalog and Lifecycle Manager,
//
//	P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
const (
	ImageStatusActive            = "ACTIVE"
	ImageStatusDeprecated        = "DEPRECATED"
	ImageStatusObsolete          = "OBSOLETE"
	ImageStatusFailed            = "FAILED"
	ImageStatusPendingValidation = "PENDING_VALIDATION"
)

// ImageIsLaunchable reports whether the given image status permits VM launch.
//
// ACTIVE and DEPRECATED are launchable.
// OBSOLETE, FAILED, and PENDING_VALIDATION are blocked.
//
// Source: vm-13-01__blueprint__ §core_contracts
//
//	"The VM creation API's admission controller MUST reject any request to create
//	 a VM from an image whose state is OBSOLETE or FAILED."
//
// P2_IMAGE_SNAPSHOT_MODEL.md §3.8: ACTIVE required; DEPRECATED still launchable.
func ImageIsLaunchable(status string) bool {
	return status == ImageStatusActive || status == ImageStatusDeprecated
}

// ── Image reads ───────────────────────────────────────────────────────────────

// GetImageByID fetches a single image by its UUID.
// Returns (nil, nil) when the image does not exist.
// Does NOT enforce visibility — use GetImageForAdmission for principal-gated access.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.
func (r *Repo) GetImageByID(ctx context.Context, id string) (*ImageRow, error) {
	row := &ImageRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, name, os_family, os_version, architecture,
		       owner_id, visibility, source_type, storage_url, min_disk_gb,
		       status, validation_status, deprecated_at, obsoleted_at,
		       source_snapshot_id, created_at, updated_at
		FROM images
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.Name, &row.OSFamily, &row.OSVersion, &row.Architecture,
		&row.OwnerID, &row.Visibility, &row.SourceType, &row.StorageURL, &row.MinDiskGB,
		&row.Status, &row.ValidationStatus, &row.DeprecatedAt, &row.ObsoletedAt,
		&row.SourceSnapshotID, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetImageByID %s: %w", id, err)
	}
	return row, nil
}

// GetImageForAdmission fetches an image by ID and enforces visibility against
// the requesting principal.
//
// Returns (image, nil) when the image exists and is visible to callerPrincipalID.
// Returns (nil, nil) when:
//   - The image does not exist.
//   - The image is PRIVATE and callerPrincipalID does not match owner_id.
//
// Status enforcement (OBSOLETE/FAILED blocking) is the caller's responsibility
// via ImageIsLaunchable(img.Status). This separation matches the fetch-then-gate
// pattern used by loadOwnedInstance and loadOwnedVolume.
//
// Source: AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-cross-account),
//
//	P2_IMAGE_SNAPSHOT_MODEL.md §3.7 (visibility rules),
//	vm-13-01__blueprint__ §core_contracts "Platform Trust Boundary".
func (r *Repo) GetImageForAdmission(ctx context.Context, id, callerPrincipalID string) (*ImageRow, error) {
	img, err := r.GetImageByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, nil
	}
	// PRIVATE images: only the owning principal may access.
	// PUBLIC images: any authenticated principal may access.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.7.
	if img.Visibility == "PRIVATE" && img.OwnerID != callerPrincipalID {
		// Return nil — caller writes 404 (not 403). Auth boundary must not leak existence.
		return nil, nil
	}
	return img, nil
}

// ListImagesByPrincipal returns all images accessible to the given principal:
//   - All PUBLIC images (regardless of owner).
//   - PRIVATE images owned by the principal.
//
// Results are ordered newest-first by created_at.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (GET /v1/images),
//
//	AUTH_OWNERSHIP_MODEL_V1.md §3.
func (r *Repo) ListImagesByPrincipal(ctx context.Context, principalID string) ([]*ImageRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, os_family, os_version, architecture,
		       owner_id, visibility, source_type, storage_url, min_disk_gb,
		       status, validation_status, deprecated_at, obsoleted_at,
		       source_snapshot_id, created_at, updated_at
		FROM images
		WHERE visibility = 'PUBLIC'
		   OR (visibility = 'PRIVATE' AND owner_id = $1)
		ORDER BY created_at DESC
	`, principalID)
	if err != nil {
		return nil, fmt.Errorf("ListImagesByPrincipal: %w", err)
	}
	defer rows.Close()

	var out []*ImageRow
	for rows.Next() {
		row := &ImageRow{}
		if err := rows.Scan(
			&row.ID, &row.Name, &row.OSFamily, &row.OSVersion, &row.Architecture,
			&row.OwnerID, &row.Visibility, &row.SourceType, &row.StorageURL, &row.MinDiskGB,
			&row.Status, &row.ValidationStatus, &row.DeprecatedAt, &row.ObsoletedAt,
			&row.SourceSnapshotID, &row.CreatedAt, &row.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListImagesByPrincipal scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
