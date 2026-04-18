package db

// image_build_manifest_repo.go — Persistence methods for image_build_manifests.
//
// VM-P3B Job 3: trusted image factory, validation, and rollout.
//
// Stores and retrieves the build manifest / digest / provenance metadata
// produced by the Image Build Service for each PLATFORM image.
//
// The manifest row is created by the IMAGE_CREATE worker immediately after
// the hermetic build completes. The signing fields (provenance_json, signature,
// signed_at) are populated by the Image Signing & Provenance worker when the
// KMS/HSM signing step completes.
//
// Source: vm-13-01__blueprint__ §Image Build Service,
//         vm-13-01__blueprint__ §Image Signing and Provenance Service,
//         vm-13-01__blueprint__ §core_contracts "Provenance Verifiability",
//         db/migrations/0015_image_validation_results.up.sql.

import (
	"context"
	"fmt"
	"time"
)

// ── ImageBuildManifestRow ──────────────────────────────────────────────────────

// ImageBuildManifestRow is the DB representation of one image_build_manifests row.
// Column order matches the SELECT list in all queries in this file.
type ImageBuildManifestRow struct {
	ImageID         string
	BuildConfigRef  string
	BaseImageDigest string
	ImageDigest     string
	ProvenanceJSON  *string    // nil until the signing service writes the attestation
	Signature       *string    // nil until signing is complete
	SignedAt        *time.Time // nil until signing is complete
	CreatedAt       time.Time
}

const selectBuildManifestCols = `
	image_id, build_config_ref, base_image_digest, image_digest,
	provenance_json, signature, signed_at, created_at`

func scanBuildManifest(row Row, r *ImageBuildManifestRow) error {
	return row.Scan(
		&r.ImageID, &r.BuildConfigRef, &r.BaseImageDigest, &r.ImageDigest,
		&r.ProvenanceJSON, &r.Signature, &r.SignedAt, &r.CreatedAt,
	)
}

// ── Reads ──────────────────────────────────────────────────────────────────────

// GetBuildManifest fetches the build manifest for the given image.
// Returns (nil, nil) when no manifest exists (image is not PLATFORM or
// the build worker has not yet written the manifest).
// Source: vm-13-01__blueprint__ §Image Signing and Provenance Service.
func (r *Repo) GetBuildManifest(ctx context.Context, imageID string) (*ImageBuildManifestRow, error) {
	row := &ImageBuildManifestRow{}
	err := scanBuildManifest(r.pool.QueryRow(ctx, `
		SELECT `+selectBuildManifestCols+`
		FROM image_build_manifests
		WHERE image_id = $1
	`, imageID), row)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetBuildManifest image=%s: %w", imageID, err)
	}
	return row, nil
}

// ── Writes ─────────────────────────────────────────────────────────────────────

// UpsertBuildManifest inserts the build manifest produced by the hermetic build
// worker. Called once per image immediately after the build artifact is produced.
//
// ON CONFLICT on image_id (primary key) updates build fields — supports
// idempotent retry by the build worker.
//
// Source: vm-13-01__blueprint__ §Image Build Service (interface_or_contract).
func (r *Repo) UpsertBuildManifest(ctx context.Context, row *ImageBuildManifestRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO image_build_manifests (
			image_id, build_config_ref, base_image_digest, image_digest, created_at
		) VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (image_id) DO UPDATE
		    SET build_config_ref  = EXCLUDED.build_config_ref,
		        base_image_digest = EXCLUDED.base_image_digest,
		        image_digest      = EXCLUDED.image_digest
	`, row.ImageID, row.BuildConfigRef, row.BaseImageDigest, row.ImageDigest)
	if err != nil {
		return fmt.Errorf("UpsertBuildManifest image=%s: %w", row.ImageID, err)
	}
	return nil
}

// SetManifestSignature records the signing outcome for a PLATFORM image.
//
// Called by the Image Signing & Provenance worker after the KMS/HSM signing
// step succeeds. Sets provenance_json (the signed SLSA L3 in-toto attestation),
// signature (the detached cryptographic signature over image_digest), and
// signed_at.
//
// Both provenanceJSON and signature must be non-empty; the caller must validate
// inputs before invoking this method.
//
// Source: vm-13-01__blueprint__ §Image Signing and Provenance Service,
//         vm-13-01__blueprint__ §core_contracts "Provenance Verifiability".
func (r *Repo) SetManifestSignature(ctx context.Context, imageID, provenanceJSON, signature string, signedAt time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE image_build_manifests
		SET provenance_json = $2,
		    signature       = $3,
		    signed_at       = $4
		WHERE image_id = $1
	`, imageID, provenanceJSON, signature, signedAt)
	if err != nil {
		return fmt.Errorf("SetManifestSignature image=%s: %w", imageID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SetManifestSignature: no manifest found for image %s", imageID)
	}
	return nil
}

// IsBuildManifestSigned reports whether the build manifest for the given image
// has a completed signing record (both provenance and signature are present).
//
// Used by the promotion gate to block IMAGE_PUBLISH jobs from starting until
// the signing step has completed.
//
// Source: vm-13-01__blueprint__ §core_contracts "Provenance Verifiability".
func (r *Repo) IsBuildManifestSigned(ctx context.Context, imageID string) (bool, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM image_build_manifests
		WHERE image_id        = $1
		  AND signature       IS NOT NULL
		  AND provenance_json IS NOT NULL
		  AND signed_at       IS NOT NULL
	`, imageID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("IsBuildManifestSigned image=%s: %w", imageID, err)
	}
	return count > 0, nil
}
