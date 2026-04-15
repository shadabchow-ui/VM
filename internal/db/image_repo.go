package db

// image_repo.go — Image table persistence methods.
//
// VM-P2C-P1: first-class image resource model for admission checks and list/get API.
// VM-P2C-P2: CreateImage, UpdateImageStatus, InsertImageJob, CountImagesBySourceSnapshot.
// VM-P2C-P3: ResolveFamilyLatest, ResolveFamilyByVersion — family alias resolution.
//
// Source: INSTANCE_MODEL_V1.md §7 (image schema, Phase 1 image model),
//         P2_IMAGE_SNAPSHOT_MODEL.md §3 (lifecycle states, visibility, custom image),
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
//
// Family resolution (VM-P2C-P3):
//   - ResolveFamilyLatest returns the highest-versioned launchable image in a family
//     that is visible to the requesting principal.
//   - ResolveFamilyByVersion returns a specific family+version image if visible.
//   - Resolution is ownership-safe: PRIVATE family images resolve only for their owner.
//   - ACTIVE and DEPRECATED are eligible candidates. OBSOLETE, FAILED, and
//     PENDING_VALIDATION are excluded from resolution results.
//   - When family_version is NULL, ordering falls back to created_at DESC.
//   - Returns (nil, nil) when no launchable candidate exists — caller writes 422.
//   Source: vm-13-01__blueprint__ §family_seam.

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
//	db/migrations/006_images.up.sql, db/migrations/007_image_custom.up.sql.
type ImageRow struct {
	ID               string
	Name             string
	OSFamily         string
	OSVersion        string
	Architecture     string
	OwnerID          string
	Visibility       string // "PUBLIC" | "PRIVATE"
	SourceType       string // "PLATFORM" | "USER" | "SNAPSHOT" | "IMPORT"
	StorageURL       string
	MinDiskGB        int
	Status           string // "ACTIVE" | "DEPRECATED" | "OBSOLETE" | "FAILED" | "PENDING_VALIDATION"
	ValidationStatus string // "pending" | "validating" | "passed" | "failed"
	DeprecatedAt     *time.Time
	ObsoletedAt      *time.Time
	SourceSnapshotID *string
	// VM-P2C-P2: custom image / import fields.
	ImportURL     *string // set for IMAGE_IMPORT jobs; nil for snapshot-sourced images
	FamilyName    *string // nil for images not belonging to a family
	FamilyVersion *int    // monotonic version within family; nil when FamilyName is nil
	CreatedAt     time.Time
	UpdatedAt     time.Time
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

// Image visibility constants.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.7.
const (
	ImageVisibilityPublic  = "PUBLIC"
	ImageVisibilityPrivate = "PRIVATE"
)

// Image source type constants.
// Source: INSTANCE_MODEL_V1.md §7, P2_IMAGE_SNAPSHOT_MODEL.md §3.
const (
	ImageSourceTypePlatform = "PLATFORM"
	ImageSourceTypeUser     = "USER"
	ImageSourceTypeSnapshot = "SNAPSHOT"
	ImageSourceTypeImport   = "IMPORT"
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

// ── selectImageCols is the canonical column list for all image SELECTs. ───────
// Order must match imageRow.Scan in the test harness (mempool_image_patch_test.go).
// VM-P2C-P2: added import_url, family_name, family_version at positions 15–17.
// Total: 20 columns.
const selectImageCols = `
	id, name, os_family, os_version, architecture,
	owner_id, visibility, source_type, storage_url, min_disk_gb,
	status, validation_status, deprecated_at, obsoleted_at,
	source_snapshot_id, import_url, family_name, family_version,
	created_at, updated_at`

// scanImage scans one image row using the selectImageCols column order.
func scanImage(row Row, r *ImageRow) error {
	return row.Scan(
		&r.ID, &r.Name, &r.OSFamily, &r.OSVersion, &r.Architecture,
		&r.OwnerID, &r.Visibility, &r.SourceType, &r.StorageURL, &r.MinDiskGB,
		&r.Status, &r.ValidationStatus, &r.DeprecatedAt, &r.ObsoletedAt,
		&r.SourceSnapshotID, &r.ImportURL, &r.FamilyName, &r.FamilyVersion,
		&r.CreatedAt, &r.UpdatedAt,
	)
}

// ── Image reads ───────────────────────────────────────────────────────────────

// GetImageByID fetches a single image by its UUID.
// Returns (nil, nil) when the image does not exist.
// Does NOT enforce visibility — use GetImageForAdmission for principal-gated access.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.
func (r *Repo) GetImageByID(ctx context.Context, id string) (*ImageRow, error) {
	row := &ImageRow{}
	err := scanImage(r.pool.QueryRow(ctx, `
		SELECT `+selectImageCols+`
		FROM images
		WHERE id = $1
	`, id), row)
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
		SELECT `+selectImageCols+`
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
		if err := scanImage(rows, row); err != nil {
			return nil, fmt.Errorf("ListImagesByPrincipal scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ── Family resolution (VM-P2C-P3) ────────────────────────────────────────────

// ResolveFamilyLatest resolves the "latest launchable image" alias for a named family.
//
// Selection rules (in order):
//  1. family_name must match exactly (case-sensitive).
//  2. Image must be visible to callerPrincipalID:
//     PUBLIC images → any caller; PRIVATE images → owner only.
//  3. Status must be ACTIVE or DEPRECATED (launchable states only).
//     OBSOLETE, FAILED, PENDING_VALIDATION are excluded.
//  4. Among candidates, ordering is: family_version DESC NULLS LAST, then created_at DESC.
//     This means versioned images (family_version IS NOT NULL) always rank above
//     unversioned images (family_version IS NULL) in the same family.
//
// Returns (nil, nil) when:
//   - The family name does not exist or has no visible images.
//   - The family exists but all images are in non-launchable states.
//
// The caller is responsible for writing the appropriate 422 error.
//
// Source: vm-13-01__blueprint__ §family_seam,
//
//	P2_IMAGE_SNAPSHOT_MODEL.md §3.8 (admission: ACTIVE or DEPRECATED only),
//	AUTH_OWNERSHIP_MODEL_V1.md §3 (visibility — 404-for-cross-account).
func (r *Repo) ResolveFamilyLatest(ctx context.Context, familyName, callerPrincipalID string) (*ImageRow, error) {
	row := &ImageRow{}
	err := scanImage(r.pool.QueryRow(ctx, `
		SELECT `+selectImageCols+`
		FROM images
		WHERE family_name = $1
		  AND (visibility = 'PUBLIC' OR (visibility = 'PRIVATE' AND owner_id = $2))
		  AND status IN ('ACTIVE', 'DEPRECATED')
		ORDER BY family_version DESC NULLS LAST, created_at DESC
		LIMIT 1
	`, familyName, callerPrincipalID), row)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ResolveFamilyLatest %q: %w", familyName, err)
	}
	return row, nil
}

// ResolveFamilyByVersion resolves a specific family+version combination.
//
// Selection rules:
//  1. family_name must match exactly.
//  2. family_version must match exactly.
//  3. Image must be visible to callerPrincipalID (PUBLIC or owned PRIVATE).
//  4. Status must be ACTIVE or DEPRECATED.
//
// Returns (nil, nil) when the version does not exist, is not visible, or is
// not in a launchable state.
//
// Source: vm-13-01__blueprint__ §family_seam.
func (r *Repo) ResolveFamilyByVersion(ctx context.Context, familyName string, version int, callerPrincipalID string) (*ImageRow, error) {
	row := &ImageRow{}
	err := scanImage(r.pool.QueryRow(ctx, `
		SELECT `+selectImageCols+`
		FROM images
		WHERE family_name = $1
		  AND family_version = $2
		  AND (visibility = 'PUBLIC' OR (visibility = 'PRIVATE' AND owner_id = $3))
		  AND status IN ('ACTIVE', 'DEPRECATED')
		LIMIT 1
	`, familyName, version, callerPrincipalID), row)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ResolveFamilyByVersion %q v%d: %w", familyName, version, err)
	}
	return row, nil
}

// ── Image writes ──────────────────────────────────────────────────────────────

// CreateImage inserts a new custom image record.
//
// The caller must set:
//   - ID, Name, OSFamily, OSVersion, Architecture
//   - OwnerID, Visibility (PRIVATE for user images)
//   - SourceType (SNAPSHOT or IMPORT)
//   - MinDiskGB
//   - Status (PENDING_VALIDATION for all new custom images)
//   - ValidationStatus ("pending")
//   - SourceSnapshotID (non-nil for SNAPSHOT source type)
//   - ImportURL (non-nil for IMPORT source type)
//
// FamilyName and FamilyVersion are optional (nil for unaffiliated images).
// StorageURL is empty at creation time for custom images — set by the worker.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (custom image creation flow).
func (r *Repo) CreateImage(ctx context.Context, row *ImageRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO images (
			id, name, os_family, os_version, architecture,
			owner_id, visibility, source_type, storage_url, min_disk_gb,
			status, validation_status,
			source_snapshot_id, import_url, family_name, family_version,
			created_at, updated_at
		) VALUES (
			$1,$2,$3,$4,$5,
			$6,$7,$8,$9,$10,
			$11,$12,
			$13,$14,$15,$16,
			NOW(),NOW()
		)
	`,
		row.ID, row.Name, row.OSFamily, row.OSVersion, row.Architecture,
		row.OwnerID, row.Visibility, row.SourceType, row.StorageURL, row.MinDiskGB,
		row.Status, row.ValidationStatus,
		row.SourceSnapshotID, row.ImportURL, row.FamilyName, row.FamilyVersion,
	)
	if err != nil {
		return fmt.Errorf("CreateImage: %w", err)
	}
	return nil
}

// UpdateImageStatus transitions an image to a new lifecycle status.
// Sets deprecated_at when transitioning to DEPRECATED.
// Sets obsoleted_at when transitioning to OBSOLETE.
//
// Source: vm-13-01__blueprint__ §core_contracts "Image Lifecycle State Enforcement",
//
//	P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
func (r *Repo) UpdateImageStatus(ctx context.Context, id, newStatus string, deprecatedAt, obsoletedAt *time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE images
		SET status        = $2,
		    deprecated_at = COALESCE($3, deprecated_at),
		    obsoleted_at  = COALESCE($4, obsoleted_at),
		    updated_at    = NOW()
		WHERE id = $1
	`, id, newStatus, deprecatedAt, obsoletedAt)
	if err != nil {
		return fmt.Errorf("UpdateImageStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateImageStatus: image %s not found", id)
	}
	return nil
}

// UpdateImageValidationStatus sets the validation_status and, on success,
// transitions the image from PENDING_VALIDATION to ACTIVE (or FAILED on failure).
// Called by the IMAGE_CREATE / IMAGE_IMPORT worker on job completion.
// Source: vm-13-01__blueprint__ §Image Catalog and Lifecycle Manager (worker transition).
func (r *Repo) UpdateImageValidationStatus(ctx context.Context, id, validationStatus, imageStatus string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE images
		SET validation_status = $2,
		    status            = $3,
		    updated_at        = NOW()
		WHERE id = $1
		  AND status = 'PENDING_VALIDATION'
	`, id, validationStatus, imageStatus)
	if err != nil {
		return fmt.Errorf("UpdateImageValidationStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateImageValidationStatus: image %s not in PENDING_VALIDATION state", id)
	}
	return nil
}

// ── Image job insertion ───────────────────────────────────────────────────────

// InsertImageJob inserts a job scoped to a custom image.
// Used for IMAGE_CREATE (snapshot→image) and IMAGE_IMPORT (url→image) job types.
// ON CONFLICT on idempotency_key does nothing — caller checks for existing job.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (async job dispatch),
//
//	JOB_MODEL_V1 §3, db/migrations/007_image_custom.up.sql.
func (r *Repo) InsertImageJob(ctx context.Context, row *JobRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, image_id, job_type, status,
			idempotency_key, attempt_count, max_attempts,
			created_at, updated_at
		) VALUES ($1,$2,$3,'pending',$4,0,$5,NOW(),NOW())
		ON CONFLICT (idempotency_key) DO NOTHING
	`,
		row.ID, row.ImageID, row.JobType,
		row.IdempotencyKey, row.MaxAttempts,
	)
	if err != nil {
		return fmt.Errorf("InsertImageJob: %w", err)
	}
	return nil
}

// ── Snapshot-image linkage guard ──────────────────────────────────────────────

// CountImagesBySourceSnapshot returns the number of non-failed images whose
// source_snapshot_id matches the given snapshot.
//
// Used to prevent deleting a snapshot that is the backing source of an active
// or pending custom image.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.10 (snapshot→image lifecycle coupling).
func (r *Repo) CountImagesBySourceSnapshot(ctx context.Context, snapshotID string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM images
		WHERE source_snapshot_id = $1
		  AND status NOT IN ('FAILED', 'OBSOLETE')
	`, snapshotID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("CountImagesBySourceSnapshot: %w", err)
	}
	return count, nil
}

// HasActiveImageJob returns true when the image already has a pending or
// in_progress job of the given type.
// Prevents double-enqueue for IMAGE_CREATE and IMAGE_IMPORT.
// Source: JOB_MODEL_V1 §idempotency.
func (r *Repo) HasActiveImageJob(ctx context.Context, imageID, jobType string) (bool, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM jobs
		WHERE image_id  = $1
		  AND job_type  = $2
		  AND status IN ('pending', 'in_progress')
	`, imageID, jobType).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("HasActiveImageJob: %w", err)
	}
	return count > 0, nil
}
