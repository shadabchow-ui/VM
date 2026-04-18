package db

// image_grant_repo.go — Persistence methods for image_share_grants.
//
// VM-P3B Job 1: cross-principal private image sharing contract.
//
// Design rules:
//   - Grants apply to PRIVATE images only; PUBLIC images need no grant.
//   - Only the image owner may create or revoke grants.
//   - Revoke is forward-looking: existing running instances are not affected.
//   - Grantee cleanup is handled by ON DELETE CASCADE on grantee_principal_id.
//     No additional cleanup logic is required here.
//   - Cross-account reads return nil — caller enforces 404 (not 403).
//     Source: AUTH_OWNERSHIP_MODEL_V1.md §3.
//
// SQL conventions match image_repo.go:
//   - Repo methods operate through r.pool (db.Pool interface).
//   - isNoRowsErr used for empty-result detection.
//   - fmt.Errorf wraps all DB errors with context.
//
// Source: VM_PHASE_ROADMAP.md (VM-P3B Job 1),
//         AUTH_OWNERSHIP_MODEL_V1.md §3,
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.7,
//         db/migrations/0013_image_share_grants.up.sql.

import (
	"context"
	"fmt"
	"time"
)

// ── ImageGrantRow ─────────────────────────────────────────────────────────────

// ImageGrantRow is the DB representation of one image_share_grants row.
type ImageGrantRow struct {
	ID                 string
	ImageID            string
	OwnerPrincipalID   string
	GranteePrincipalID string
	CreatedAt          time.Time
}

// ── Image grant reads ─────────────────────────────────────────────────────────

// GetImageGrant fetches a single grant by image and grantee.
// Returns (nil, nil) when no such grant exists.
// Used by GetImageForAdmissionWithGrants to check grantee access.
// Source: AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-cross-account).
func (r *Repo) GetImageGrant(ctx context.Context, imageID, granteePrincipalID string) (*ImageGrantRow, error) {
	row := &ImageGrantRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, image_id, owner_principal_id, grantee_principal_id, created_at
		FROM image_share_grants
		WHERE image_id            = $1
		  AND grantee_principal_id = $2
	`, imageID, granteePrincipalID).Scan(
		&row.ID, &row.ImageID, &row.OwnerPrincipalID, &row.GranteePrincipalID, &row.CreatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetImageGrant image=%s grantee=%s: %w", imageID, granteePrincipalID, err)
	}
	return row, nil
}

// ListImageGrants returns all grants for a given image, ordered by created_at ASC.
// Only the owning principal should be given the result; caller enforces that.
// Source: VM-P3B Job 1 §3 (owner-only list).
func (r *Repo) ListImageGrants(ctx context.Context, imageID string) ([]*ImageGrantRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, image_id, owner_principal_id, grantee_principal_id, created_at
		FROM image_share_grants
		WHERE image_id = $1
		ORDER BY created_at ASC
	`, imageID)
	if err != nil {
		return nil, fmt.Errorf("ListImageGrants image=%s: %w", imageID, err)
	}
	defer rows.Close()

	var out []*ImageGrantRow
	for rows.Next() {
		g := &ImageGrantRow{}
		if err := rows.Scan(&g.ID, &g.ImageID, &g.OwnerPrincipalID, &g.GranteePrincipalID, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListImageGrants scan: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ── Image grant writes ────────────────────────────────────────────────────────

// CreateImageGrant inserts a share grant row.
// ON CONFLICT (image_id, grantee_principal_id) DO NOTHING makes this
// idempotent: re-granting the same grantee returns nil (no error).
// Source: db/migrations/0013_image_share_grants.up.sql.
func (r *Repo) CreateImageGrant(ctx context.Context, row *ImageGrantRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO image_share_grants (id, image_id, owner_principal_id, grantee_principal_id, created_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (image_id, grantee_principal_id) DO NOTHING
	`, row.ID, row.ImageID, row.OwnerPrincipalID, row.GranteePrincipalID)
	if err != nil {
		return fmt.Errorf("CreateImageGrant image=%s grantee=%s: %w", row.ImageID, row.GranteePrincipalID, err)
	}
	return nil
}

// RevokeImageGrant deletes the grant for a specific grantee on a specific image.
// Returns (true, nil)  when a grant was deleted.
// Returns (false, nil) when no grant existed (idempotent).
// Source: VM-P3B Job 1 §8 (revoke semantics — future access only).
func (r *Repo) RevokeImageGrant(ctx context.Context, imageID, granteePrincipalID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM image_share_grants
		WHERE image_id            = $1
		  AND grantee_principal_id = $2
	`, imageID, granteePrincipalID)
	if err != nil {
		return false, fmt.Errorf("RevokeImageGrant image=%s grantee=%s: %w", imageID, granteePrincipalID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ── Extended image reads ──────────────────────────────────────────────────────

// GetImageForAdmissionWithGrants fetches an image by ID and enforces
// visibility against the requesting principal, including grant-based access.
//
// Returns (image, nil) when:
//   - The image is PUBLIC (any principal), OR
//   - The image is PRIVATE and callerPrincipalID == owner_id, OR
//   - The image is PRIVATE and an active grant exists for callerPrincipalID.
//
// Returns (nil, nil) when:
//   - The image does not exist.
//   - The image is PRIVATE, the caller is not the owner, and no grant exists.
//
// Callers must still enforce ImageIsLaunchable(img.Status) for launch admission.
//
// This replaces direct calls to GetImageForAdmission at the handler layer for
// paths that need share-grant visibility (GET /v1/images/{id}, launch admission).
//
// Source: AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-cross-account),
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.7,
//         VM-P3B Job 1 §3, §5, §6.
func (r *Repo) GetImageForAdmissionWithGrants(ctx context.Context, id, callerPrincipalID string) (*ImageRow, error) {
	img, err := r.GetImageByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, nil
	}
	if img.Visibility == ImageVisibilityPublic {
		return img, nil
	}
	// PRIVATE: owner has full access.
	if img.OwnerID == callerPrincipalID {
		return img, nil
	}
	// PRIVATE: check for an explicit share grant.
	grant, err := r.GetImageGrant(ctx, id, callerPrincipalID)
	if err != nil {
		return nil, err
	}
	if grant != nil {
		return img, nil
	}
	// No access — return nil; caller writes 404.
	return nil, nil
}

// ListImagesByPrincipalWithGrants returns all images accessible to the caller:
//   - All PUBLIC images (regardless of owner).
//   - PRIVATE images owned by the caller.
//   - PRIVATE images for which an active share grant exists for the caller.
//
// Results are ordered newest-first by created_at.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4, VM-P3B Job 1 §4.
func (r *Repo) ListImagesByPrincipalWithGrants(ctx context.Context, principalID string) ([]*ImageRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+selectImageCols+`
		FROM images
		WHERE visibility = 'PUBLIC'
		   OR (visibility = 'PRIVATE' AND owner_id = $1)
		   OR (
		        visibility = 'PRIVATE'
		        AND id IN (
		            SELECT image_id
		            FROM image_share_grants
		            WHERE grantee_principal_id = $1
		        )
		      )
		ORDER BY created_at DESC
	`, principalID)
	if err != nil {
		return nil, fmt.Errorf("ListImagesByPrincipalWithGrants: %w", err)
	}
	defer rows.Close()

	var out []*ImageRow
	for rows.Next() {
		row := &ImageRow{}
		if err := scanImage(rows, row); err != nil {
			return nil, fmt.Errorf("ListImagesByPrincipalWithGrants scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
