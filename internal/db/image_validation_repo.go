package db

// image_validation_repo.go — Persistence methods for image validation results
// and the IMAGE_VALIDATE job type.
//
// VM-P3B Job 3: trusted image factory, validation, and rollout.
//
// This file provides:
//   - ImageValidationResultRow + CRUD for image_validation_results table.
//   - InsertValidationJob — enqueues an IMAGE_VALIDATE job (extends InsertImageJob pattern).
//   - RecordValidationStage — inserts one stage result row.
//   - AllStagesPassed — promotion gate: returns true iff all required stages passed.
//   - PromoteValidatedImage — transitions PENDING_VALIDATION→ACTIVE only if all
//     required stages have passed (single atomic update).
//   - FailValidatedImage — transitions image to FAILED + records reason.
//
// The validation state machine (per blueprint §Image Validation Service):
//
//   IMAGE_VALIDATE job claimed
//        │
//        ├─ for each stage: RecordValidationStage(stage, "pass"|"fail")
//        │
//        ├─ all "pass" → PromoteValidatedImage → ACTIVE
//        └─ any "fail" → FailValidatedImage   → FAILED
//
// The IMAGE_VALIDATE worker is responsible for:
//   1. Running each stage test.
//   2. Calling RecordValidationStage for every stage.
//   3. Calling AllStagesPassed to determine the overall outcome.
//   4. Calling PromoteValidatedImage or FailValidatedImage accordingly.
//
// Source: vm-13-01__blueprint__ §Image Validation Service,
//         vm-13-01__blueprint__ §Image Catalog and Lifecycle Manager,
//         db/migrations/0015_image_validation_results.up.sql.

import (
	"context"
	"fmt"
	"time"
)

// ── Job type constants ────────────────────────────────────────────────────────

// Job type constants for the image factory pipeline.
// These extend the IMAGE_CREATE / IMAGE_IMPORT constants in image_repo.go.
// Source: vm-13-01__blueprint__ §components, JOB_MODEL_V1 §3.
const (
	// ImageJobTypeValidate is enqueued after a build manifest is signed.
	// The worker runs all validation stages and calls PromoteValidatedImage
	// or FailValidatedImage based on the aggregate result.
	ImageJobTypeValidate = "IMAGE_VALIDATE"

	// ImageJobTypePublish is enqueued after IMAGE_VALIDATE succeeds.
	// The worker drives the staged rollout and atomically updates the
	// family alias on completion.
	ImageJobTypePublish = "IMAGE_PUBLISH"
)

// Validation stage constants.
// Source: vm-13-01__blueprint__ §Image Validation Service
//         (interface_or_contract: "boot, agent health, security, integrity, performance").
const (
	ValidationStageBoot        = "boot"
	ValidationStageHealth      = "health"
	ValidationStageSecurity    = "security"
	ValidationStageIntegrity   = "integrity"
	ValidationStagePerformance = "performance" // future phase; included for completeness
)

// RequiredValidationStages is the ordered set of stages that MUST all pass
// before an image can be promoted to ACTIVE status.
// Performance is excluded from the required set (future phase per blueprint MVP).
// Source: vm-13-01__blueprint__ §mvp (deferred: "Performance Baseline Testing").
var RequiredValidationStages = []string{
	ValidationStageBoot,
	ValidationStageHealth,
	ValidationStageSecurity,
	ValidationStageIntegrity,
}

// Validation result constants.
const (
	ValidationResultPass = "pass"
	ValidationResultFail = "fail"
)

// ── ImageValidationResultRow ──────────────────────────────────────────────────

// ImageValidationResultRow is the DB representation of one image_validation_results row.
type ImageValidationResultRow struct {
	ID         string
	ImageID    string
	JobID      string
	Stage      string
	Result     string
	DetailJSON *string
	RecordedAt time.Time
}

// ── Reads ─────────────────────────────────────────────────────────────────────

// ListValidationResults returns all recorded validation stage results for an image,
// ordered by recorded_at ASC.
// Used by the worker to check current state and by the promotion gate.
// Source: vm-13-01__blueprint__ §Image Validation Service.
func (r *Repo) ListValidationResults(ctx context.Context, imageID string) ([]*ImageValidationResultRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, image_id, job_id, stage, result, detail_json, recorded_at
		FROM image_validation_results
		WHERE image_id = $1
		ORDER BY recorded_at ASC
	`, imageID)
	if err != nil {
		return nil, fmt.Errorf("ListValidationResults image=%s: %w", imageID, err)
	}
	defer rows.Close()

	var out []*ImageValidationResultRow
	for rows.Next() {
		row := &ImageValidationResultRow{}
		if err := rows.Scan(
			&row.ID, &row.ImageID, &row.JobID, &row.Stage, &row.Result,
			&row.DetailJSON, &row.RecordedAt,
		); err != nil {
			return nil, fmt.Errorf("ListValidationResults scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// AllStagesPassed returns true when all stages in RequiredValidationStages have
// at least one recorded result of "pass" AND no "fail" result exists for any
// required stage.
//
// The logic: for each required stage, the most recent result must be "pass".
// This allows a re-run of a failed stage (e.g. after a transient infrastructure
// failure) to produce a "pass" and unlock promotion.
//
// Returns (false, nil) when results are incomplete or any required stage failed.
// Returns (false, err) on DB error.
//
// Source: vm-13-01__blueprint__ §Image Validation Service.
func (r *Repo) AllStagesPassed(ctx context.Context, imageID string) (bool, error) {
	// Fetch the latest result per required stage in one query.
	// DISTINCT ON (stage) with ORDER BY recorded_at DESC gives the most recent
	// result for each stage.
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT ON (stage) stage, result
		FROM image_validation_results
		WHERE image_id = $1
		  AND stage IN ('boot','health','security','integrity')
		ORDER BY stage, recorded_at DESC
	`, imageID)
	if err != nil {
		return false, fmt.Errorf("AllStagesPassed image=%s: %w", imageID, err)
	}
	defer rows.Close()

	latest := make(map[string]string) // stage → latest result
	for rows.Next() {
		var stage, result string
		if err := rows.Scan(&stage, &result); err != nil {
			return false, fmt.Errorf("AllStagesPassed scan: %w", err)
		}
		latest[stage] = result
	}
	if err := rows.Err(); err != nil {
		return false, err
	}

	for _, stage := range RequiredValidationStages {
		result, ok := latest[stage]
		if !ok || result != ValidationResultPass {
			return false, nil
		}
	}
	return true, nil
}

// ── Writes ────────────────────────────────────────────────────────────────────

// InsertValidationJob enqueues an IMAGE_VALIDATE job for a given image.
//
// Follows the same pattern as InsertImageJob in image_repo.go.
// ON CONFLICT on idempotency_key does nothing — the caller must check
// HasActiveImageJob before enqueueing to avoid silent deduplication.
//
// Source: JOB_MODEL_V1 §idempotency, vm-13-01__blueprint__ §Image Validation Service.
func (r *Repo) InsertValidationJob(ctx context.Context, row *JobRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, image_id, job_type, status,
			idempotency_key, attempt_count, max_attempts,
			created_at, updated_at
		) VALUES ($1,$2,$3,'pending',$4,0,$5,NOW(),NOW())
		ON CONFLICT (idempotency_key) DO NOTHING
	`,
		row.ID, row.ImageID, ImageJobTypeValidate,
		row.IdempotencyKey, row.MaxAttempts,
	)
	if err != nil {
		return fmt.Errorf("InsertValidationJob: %w", err)
	}
	return nil
}

// InsertPublishJob enqueues an IMAGE_PUBLISH job for a given image.
// Follows the same pattern as InsertImageJob / InsertValidationJob.
// Source: JOB_MODEL_V1 §idempotency, vm-13-01__blueprint__ §Publication and Rollout Orchestrator.
func (r *Repo) InsertPublishJob(ctx context.Context, row *JobRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, image_id, job_type, status,
			idempotency_key, attempt_count, max_attempts,
			created_at, updated_at
		) VALUES ($1,$2,$3,'pending',$4,0,$5,NOW(),NOW())
		ON CONFLICT (idempotency_key) DO NOTHING
	`,
		row.ID, row.ImageID, ImageJobTypePublish,
		row.IdempotencyKey, row.MaxAttempts,
	)
	if err != nil {
		return fmt.Errorf("InsertPublishJob: %w", err)
	}
	return nil
}

// RecordValidationStage inserts one stage result row.
//
// The id field must be a unique ID (idgen prefix "ivr"). DetailJSON is optional
// (nil when the stage produces no structured output).
//
// Multiple results for the same stage are allowed — they represent re-runs.
// AllStagesPassed uses the most recent result per stage.
//
// Source: vm-13-01__blueprint__ §Image Validation Service.
func (r *Repo) RecordValidationStage(ctx context.Context, row *ImageValidationResultRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO image_validation_results (id, image_id, job_id, stage, result, detail_json, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, row.ID, row.ImageID, row.JobID, row.Stage, row.Result, row.DetailJSON)
	if err != nil {
		return fmt.Errorf("RecordValidationStage image=%s stage=%s: %w", row.ImageID, row.Stage, err)
	}
	return nil
}

// PromoteValidatedImage atomically transitions an image from PENDING_VALIDATION
// to ACTIVE, but only if all required validation stages have passed.
//
// The promotion gate is enforced at the DB layer:
//   - WHERE status = 'PENDING_VALIDATION' prevents double-promotion.
//   - The caller must also call AllStagesPassed before invoking this method;
//     the DB-level WHERE clause is a safety net, not a replacement.
//
// Returns ErrPromotionNotReady (non-nil, distinct error type) when the image
// is not in PENDING_VALIDATION state, so the caller can distinguish "already
// promoted" from a genuine DB error.
//
// Source: vm-13-01__blueprint__ §Image Catalog and Lifecycle Manager,
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.4 (lifecycle transitions).
func (r *Repo) PromoteValidatedImage(ctx context.Context, imageID string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE images
		SET status            = 'ACTIVE',
		    validation_status = 'passed',
		    updated_at        = NOW()
		WHERE id     = $1
		  AND status = 'PENDING_VALIDATION'
	`, imageID)
	if err != nil {
		return fmt.Errorf("PromoteValidatedImage image=%s: %w", imageID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("PromoteValidatedImage: image %s is not in PENDING_VALIDATION state (already promoted or does not exist)", imageID)
	}
	return nil
}

// FailValidatedImage transitions an image to FAILED status.
//
// Used by the validation worker when any required stage fails.
// Also used by the rollout worker on rollback (FailRolledBackImage below).
//
// The WHERE clause constrains to PENDING_VALIDATION to prevent accidentally
// failing an already-ACTIVE image during a race condition.
//
// Source: vm-13-01__blueprint__ §Image Validation Service,
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
func (r *Repo) FailValidatedImage(ctx context.Context, imageID string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE images
		SET status            = 'FAILED',
		    validation_status = 'failed',
		    updated_at        = NOW()
		WHERE id     = $1
		  AND status = 'PENDING_VALIDATION'
	`, imageID)
	if err != nil {
		return fmt.Errorf("FailValidatedImage image=%s: %w", imageID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("FailValidatedImage: image %s not in PENDING_VALIDATION state", imageID)
	}
	return nil
}
