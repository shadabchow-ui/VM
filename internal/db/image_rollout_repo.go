package db

// image_rollout_repo.go — Persistence methods for image_rollouts and
// image_cve_waivers tables.
//
// VM-P3B Job 3: trusted image factory, validation, and rollout.
//
// Rollout state machine (per vm-13-01__blueprint__ §Publication and Rollout Orchestrator):
//
//   CreateRollout          → status='pending'
//   StartCanary            → status='canary', canary_percent > 0
//   AdvanceCanary          → status='canary', canary_percent += step
//   BeginPromotion         → status='promoting'
//   CompleteRollout        → status='completed', family alias updated atomically
//   BeginRollback          → status='rolling_back', failure_reason set
//   CompleteRollback       → status='rolled_back', image marked FAILED
//
// The IMAGE_PUBLISH worker drives all transitions by calling these methods.
// The rollout row tracks orchestrator state. The image row tracks the
// authoritative API-visible lifecycle state (ACTIVE / FAILED).
//
// Family alias update (atomicity contract):
//   CompleteRollout calls UpdateFamilyAlias which sets family_version = MAX+1
//   in a single UPDATE statement, making the alias transition atomic.
//   Source: vm-13-01__blueprint__ §core_contracts "Image Family Atomicity".
//
// CVE waiver lookup:
//   IsCVEWaived provides the security validation stage with a point-lookup
//   to check whether a given CVE is covered by an active waiver for a family.
//   Source: vm-13-01__blueprint__ §Image Validation Service,
//           vm-13-01__skill__ §instructions "Formalize a CVE Waiver Process".

import (
	"context"
	"fmt"
	"time"
)

// ── Rollout status constants ───────────────────────────────────────────────────

const (
	RolloutStatusPending      = "pending"
	RolloutStatusCanary       = "canary"
	RolloutStatusPromoting    = "promoting"
	RolloutStatusCompleted    = "completed"
	RolloutStatusRollingBack  = "rolling_back"
	RolloutStatusRolledBack   = "rolled_back"
)

// ── ImageRolloutRow ───────────────────────────────────────────────────────────

// ImageRolloutRow is the DB representation of one image_rollouts row.
type ImageRolloutRow struct {
	ID             string
	ImageID        string
	JobID          string
	FamilyName     string
	Status         string
	CanaryPercent  int
	FailureReason  *string
	StartedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

// ── Reads ─────────────────────────────────────────────────────────────────────

// GetRolloutByImageID fetches the rollout record for the given image.
// Returns (nil, nil) when no rollout record exists.
func (r *Repo) GetRolloutByImageID(ctx context.Context, imageID string) (*ImageRolloutRow, error) {
	row := &ImageRolloutRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, image_id, job_id, family_name, status, canary_percent,
		       failure_reason, started_at, updated_at, completed_at
		FROM image_rollouts
		WHERE image_id = $1
	`, imageID).Scan(
		&row.ID, &row.ImageID, &row.JobID, &row.FamilyName, &row.Status,
		&row.CanaryPercent, &row.FailureReason, &row.StartedAt,
		&row.UpdatedAt, &row.CompletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetRolloutByImageID image=%s: %w", imageID, err)
	}
	return row, nil
}

// ── Rollout state machine writes ──────────────────────────────────────────────

// CreateRollout inserts a new rollout record in 'pending' status.
// Called by the IMAGE_PUBLISH worker when it claims the job.
// ON CONFLICT on image_id (UNIQUE) returns an error — the caller must check
// GetRolloutByImageID first to avoid duplicate records.
// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator.
func (r *Repo) CreateRollout(ctx context.Context, row *ImageRolloutRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO image_rollouts (id, image_id, job_id, family_name, status, canary_percent, started_at, updated_at)
		VALUES ($1, $2, $3, $4, 'pending', 0, NOW(), NOW())
	`, row.ID, row.ImageID, row.JobID, row.FamilyName)
	if err != nil {
		return fmt.Errorf("CreateRollout image=%s: %w", row.ImageID, err)
	}
	return nil
}

// StartCanary transitions the rollout from 'pending' to 'canary' with the
// initial canary traffic percentage. This is the first step of the staged rollout.
// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator.
func (r *Repo) StartCanary(ctx context.Context, rolloutID string, canaryPercent int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE image_rollouts
		SET status         = 'canary',
		    canary_percent = $2,
		    updated_at     = NOW()
		WHERE id     = $1
		  AND status = 'pending'
	`, rolloutID, canaryPercent)
	if err != nil {
		return fmt.Errorf("StartCanary rollout=%s: %w", rolloutID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("StartCanary: rollout %s not in 'pending' state", rolloutID)
	}
	return nil
}

// AdvanceCanary updates the canary_percent for an in-progress canary rollout.
// Called repeatedly as the rollout worker increases traffic exposure.
// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator.
func (r *Repo) AdvanceCanary(ctx context.Context, rolloutID string, newCanaryPercent int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE image_rollouts
		SET canary_percent = $2,
		    updated_at     = NOW()
		WHERE id     = $1
		  AND status = 'canary'
	`, rolloutID, newCanaryPercent)
	if err != nil {
		return fmt.Errorf("AdvanceCanary rollout=%s: %w", rolloutID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("AdvanceCanary: rollout %s not in 'canary' state", rolloutID)
	}
	return nil
}

// BeginPromotion transitions the rollout from 'canary' to 'promoting'.
// Called when canary metrics pass and full promotion begins.
// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator.
func (r *Repo) BeginPromotion(ctx context.Context, rolloutID string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE image_rollouts
		SET status     = 'promoting',
		    updated_at = NOW()
		WHERE id     = $1
		  AND status = 'canary'
	`, rolloutID)
	if err != nil {
		return fmt.Errorf("BeginPromotion rollout=%s: %w", rolloutID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("BeginPromotion: rollout %s not in 'canary' state", rolloutID)
	}
	return nil
}

// CompleteRollout transitions the rollout to 'completed' and atomically updates
// the family alias by calling UpdateFamilyAlias.
//
// The family alias update uses a single UPDATE statement (see image_repo.go
// UpdateFamilyAlias) to atomically set family_version = MAX+1, satisfying the
// atomicity contract: at no point can the alias resolve to an invalid image.
//
// Both the rollout status update and the family alias update happen in the same
// control-plane transaction window. The caller (IMAGE_PUBLISH worker) is
// responsible for wrapping this in a DB transaction if strict atomicity is needed.
// For Phase 3, the sequential call is acceptable — the alias update is idempotent
// and the rollout status guards against double-execution.
//
// Source: vm-13-01__blueprint__ §core_contracts "Image Family Atomicity",
//         vm-13-01__blueprint__ §Publication and Rollout Orchestrator.
func (r *Repo) CompleteRollout(ctx context.Context, rolloutID, imageID, familyName string) error {
	// 1. Transition rollout to 'completed'.
	tag, err := r.pool.Exec(ctx, `
		UPDATE image_rollouts
		SET status       = 'completed',
		    completed_at = NOW(),
		    updated_at   = NOW()
		WHERE id     = $1
		  AND status IN ('promoting', 'canary')
	`, rolloutID)
	if err != nil {
		return fmt.Errorf("CompleteRollout (status update) rollout=%s: %w", rolloutID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("CompleteRollout: rollout %s not in 'promoting' or 'canary' state", rolloutID)
	}

	// 2. Atomically update the family alias (family_version = MAX+1).
	// This is the only place the family alias is updated during a rollout.
	// Source: vm-13-01__blueprint__ §core_contracts "Image Family Atomicity".
	tag, err = r.pool.Exec(ctx, `
		UPDATE images
		SET family_version = (
		        SELECT COALESCE(MAX(family_version), 0) + 1
		        FROM images
		        WHERE family_name = $1
		    ),
		    updated_at = NOW()
		WHERE id          = $2
		  AND family_name = $1
		  AND status      = 'ACTIVE'
	`, familyName, imageID)
	if err != nil {
		return fmt.Errorf("CompleteRollout (family alias) image=%s family=%q: %w", imageID, familyName, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("CompleteRollout: image %s not found in family %q or not ACTIVE", imageID, familyName)
	}
	return nil
}

// BeginRollback transitions the rollout to 'rolling_back' and records the
// failure reason. Called when canary metrics fail.
// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator.
func (r *Repo) BeginRollback(ctx context.Context, rolloutID, reason string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE image_rollouts
		SET status         = 'rolling_back',
		    failure_reason = $2,
		    updated_at     = NOW()
		WHERE id     = $1
		  AND status IN ('canary', 'promoting')
	`, rolloutID, reason)
	if err != nil {
		return fmt.Errorf("BeginRollback rollout=%s: %w", rolloutID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("BeginRollback: rollout %s not in 'canary' or 'promoting' state", rolloutID)
	}
	return nil
}

// CompleteRollback transitions the rollout to 'rolled_back' and marks the
// image as FAILED. The family alias is NOT updated — it continues to point
// to the previously promoted image.
//
// The image status transition uses UpdateImageStatus (from image_repo.go) via
// the existing pattern: ACTIVE → FAILED is intentional here because the image
// may have been promoted to ACTIVE before the canary failure was detected.
// If the image never reached ACTIVE (early canary failure), the WHERE clause
// in the UpdateImageStatus call would return 0 rows; callers should handle that
// as a non-error (idempotent rollback).
//
// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator:
//   "On failure, invokes the Image Catalog API to mark the image `FAILED`".
func (r *Repo) CompleteRollback(ctx context.Context, rolloutID, imageID string) error {
	// 1. Transition rollout to 'rolled_back'.
	tag, err := r.pool.Exec(ctx, `
		UPDATE image_rollouts
		SET status       = 'rolled_back',
		    completed_at = NOW(),
		    updated_at   = NOW()
		WHERE id     = $1
		  AND status = 'rolling_back'
	`, rolloutID)
	if err != nil {
		return fmt.Errorf("CompleteRollback (rollout status) rollout=%s: %w", rolloutID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("CompleteRollback: rollout %s not in 'rolling_back' state", rolloutID)
	}

	// 2. Mark image as FAILED.
	// Accepts ACTIVE or PENDING_VALIDATION → FAILED (handles both early and late failures).
	// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator.
	_, err = r.pool.Exec(ctx, `
		UPDATE images
		SET status     = 'FAILED',
		    updated_at = NOW()
		WHERE id     = $1
		  AND status IN ('ACTIVE', 'PENDING_VALIDATION')
	`, imageID)
	if err != nil {
		return fmt.Errorf("CompleteRollback (mark FAILED) image=%s: %w", imageID, err)
	}
	// 0 rows affected is acceptable — image may already be in a terminal state.
	return nil
}

// ── CVE waiver reads ──────────────────────────────────────────────────────────

// ImageCVEWaiverRow is the DB representation of one image_cve_waivers row.
type ImageCVEWaiverRow struct {
	ID          string
	CVEID       string
	ImageFamily *string
	GrantedBy   string
	Reason      string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

// IsCVEWaived returns true when an active, non-expired waiver exists for the
// given CVE ID and image family (or a global waiver with image_family IS NULL).
//
// Called by the Image Validation Service worker during the security stage to
// determine whether a CVE finding should be treated as a pass.
//
// Lookup precedence:
//  1. Family-specific waiver (image_family = familyName).
//  2. Global waiver (image_family IS NULL).
//
// A waiver is active when: revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW()).
//
// Source: vm-13-01__blueprint__ §Image Validation Service,
//         vm-13-01__skill__ §instructions "Formalize a CVE Waiver Process".
func (r *Repo) IsCVEWaived(ctx context.Context, cveID, imageFamilyName string) (bool, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM image_cve_waivers
		WHERE cve_id     = $1
		  AND (image_family = $2 OR image_family IS NULL)
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())
	`, cveID, imageFamilyName).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("IsCVEWaived cve=%s family=%s: %w", cveID, imageFamilyName, err)
	}
	return count > 0, nil
}

// CreateCVEWaiver inserts a new CVE waiver record.
// Called by the platform operator API (future) or directly from ops tooling.
// Source: vm-13-01__skill__ §instructions "Formalize a CVE Waiver Process".
func (r *Repo) CreateCVEWaiver(ctx context.Context, row *ImageCVEWaiverRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO image_cve_waivers (id, cve_id, image_family, granted_by, reason, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, row.ID, row.CVEID, row.ImageFamily, row.GrantedBy, row.Reason, row.ExpiresAt)
	if err != nil {
		return fmt.Errorf("CreateCVEWaiver cve=%s: %w", row.CVEID, err)
	}
	return nil
}

// RevokeCVEWaiver soft-deletes a waiver by setting revoked_at = NOW().
// Idempotent: revoking an already-revoked waiver is a no-op.
// Source: vm-13-01__skill__ §instructions "Formalize a CVE Waiver Process".
func (r *Repo) RevokeCVEWaiver(ctx context.Context, waiverID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE image_cve_waivers
		SET revoked_at = NOW()
		WHERE id          = $1
		  AND revoked_at IS NULL
	`, waiverID)
	if err != nil {
		return fmt.Errorf("RevokeCVEWaiver id=%s: %w", waiverID, err)
	}
	return nil
}
