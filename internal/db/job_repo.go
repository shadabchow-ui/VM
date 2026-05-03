package db

// job_repo.go — Job table persistence methods.
//
// Source: JOB_MODEL_V1 §1 (schema), §2 (states), §3 (types),
//         IMPLEMENTATION_PLAN_V1 §20 (Job CRUD: create, get_by_id, get_by_idempotency_key,
//         atomic_claim, update_status).
//
// PASS 3: Added GetJobByInstanceAndID for the job-status endpoint.
// VM-P2B: Added VolumeID field to JobRow for volume-scoped jobs.
//         InsertVolumeJob lives in volume_repo.go.
//         ListStuckInProgressJobs extended with volume job timeout intervals.
// VM-P2B-S2: Added SnapshotID field to JobRow for snapshot-scoped jobs.
//         InsertSnapshotJob lives in snapshot_repo.go.
// VM-P2C: Added ImageID field to JobRow for image-scoped jobs.
//         InsertImageJob lives in image_repo.go.

import (
	"context"
	"fmt"
	"time"
)

// JobRow is the DB representation of a job record.
// VM-P2B: Added VolumeID — nullable FK to volumes.
// VM-P2B-S2: Added SnapshotID — nullable FK to snapshots.
// VM-P2C: Added ImageID — nullable FK to images.
// Invariant: exactly one of InstanceID, VolumeID, SnapshotID, or ImageID is non-null
// (enforced at the application layer; the DB allows any combination).
// Source: JOB_MODEL_V1 §1, P2_VOLUME_MODEL.md §4.2, P2_IMAGE_SNAPSHOT_MODEL.md §4.
type JobRow struct {
	ID             string
	InstanceID     string  // empty string when VolumeID, SnapshotID, or ImageID is set
	VolumeID       *string // non-nil for VOLUME_* job types
	SnapshotID     *string // non-nil for SNAPSHOT_* job types
	ImageID        *string // non-nil for IMAGE_* job types
	JobType        string
	Status         string
	IdempotencyKey string
	AttemptCount   int
	MaxAttempts    int
	ErrorMessage   *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClaimedAt      *time.Time
	CompletedAt    *time.Time
}

// InsertJob creates a new job in 'pending' status.
// ON CONFLICT on idempotency_key does nothing — caller checks for existing job.
// Source: JOB_MODEL_V1 §1, 03-02-async-job-model §idempotency.
// Note: This method is for instance-scoped jobs only. For volume-scoped jobs,
// use InsertVolumeJob in volume_repo.go. For snapshot-scoped jobs,
// use InsertSnapshotJob in snapshot_repo.go.
func (r *Repo) InsertJob(ctx context.Context, row *JobRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, instance_id, job_type, status,
			idempotency_key, attempt_count, max_attempts,
			created_at, updated_at
		) VALUES ($1,$2,$3,'pending',$4,0,$5,NOW(),NOW())
		ON CONFLICT (idempotency_key) DO NOTHING
	`,
		row.ID, row.InstanceID, row.JobType,
		row.IdempotencyKey, row.MaxAttempts,
	)
	if err != nil {
		return fmt.Errorf("InsertJob: %w", err)
	}
	return nil
}

// GetJobByID fetches a job by primary key.
// Returns nil, nil when not found.
func (r *Repo) GetJobByID(ctx context.Context, id string) (*JobRow, error) {
	row := &JobRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, volume_id, snapshot_id,
		       COALESCE(image_id, NULL)::VARCHAR(64) AS image_id,
		       job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.InstanceID, &row.VolumeID, &row.SnapshotID, &row.ImageID, &row.JobType, &row.Status,
		&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
		&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetJobByID: %w", err)
	}
	return row, nil
}

// GetJobByInstanceAndID fetches a job by its ID and instance_id together.
// Used by the job-status endpoint to enforce instance ownership.
// Returns nil, nil when not found.
func (r *Repo) GetJobByInstanceAndID(ctx context.Context, instanceID, jobID string) (*JobRow, error) {
	row := &JobRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, volume_id, snapshot_id,
		       COALESCE(image_id, NULL)::VARCHAR(64) AS image_id,
		       job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs
		WHERE id          = $1
		  AND instance_id = $2
	`, jobID, instanceID).Scan(
		&row.ID, &row.InstanceID, &row.VolumeID, &row.SnapshotID, &row.ImageID, &row.JobType, &row.Status,
		&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
		&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetJobByInstanceAndID: %w", err)
	}
	return row, nil
}

// GetJobByIdempotencyKey fetches a job by its idempotency key.
// Returns nil, nil when not found.
func (r *Repo) GetJobByIdempotencyKey(ctx context.Context, key string) (*JobRow, error) {
	row := &JobRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, volume_id, snapshot_id,
		       COALESCE(image_id, NULL)::VARCHAR(64) AS image_id,
		       job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs
		WHERE idempotency_key = $1
	`, key).Scan(
		&row.ID, &row.InstanceID, &row.VolumeID, &row.SnapshotID, &row.ImageID, &row.JobType, &row.Status,
		&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
		&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetJobByIdempotencyKey: %w", err)
	}
	return row, nil
}

// AtomicClaimJob claims the next pending job using SELECT FOR UPDATE SKIP LOCKED.
// Returns nil, nil when there are no pending jobs.
// Source: JOB_MODEL_V1 §atomic_claim, 03-02-async-job-model §claim.
func (r *Repo) AtomicClaimJob(ctx context.Context) (*JobRow, error) {
	row := &JobRow{}
	err := r.pool.QueryRow(ctx, `
		UPDATE jobs
		SET status        = 'in_progress',
		    attempt_count = attempt_count + 1,
		    claimed_at    = NOW(),
		    updated_at    = NOW()
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'pending'
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, instance_id, volume_id, snapshot_id,
		          COALESCE(image_id, NULL)::VARCHAR(64) AS image_id,
		          job_type, status,
		          idempotency_key, attempt_count, max_attempts,
		          error_message, created_at, updated_at, claimed_at, completed_at
	`).Scan(
		&row.ID, &row.InstanceID, &row.VolumeID, &row.SnapshotID, &row.ImageID, &row.JobType, &row.Status,
		&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
		&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("AtomicClaimJob: %w", err)
	}
	return row, nil
}

// UpdateJobStatus sets a job's terminal or requeue status.
// errorMessage is optional — pass nil for success transitions.
// Source: JOB_MODEL_V1 §2 (states: completed, dead).
func (r *Repo) UpdateJobStatus(ctx context.Context, id, status string, errorMessage *string) error {
	var completedAt *time.Time
	if status == "completed" || status == "dead" {
		now := time.Now()
		completedAt = &now
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs
		SET status        = $2,
		    error_message = $3,
		    completed_at  = $4,
		    updated_at    = NOW()
		WHERE id = $1
	`, id, status, errorMessage, completedAt)
	if err != nil {
		return fmt.Errorf("UpdateJobStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateJobStatus: job %s not found", id)
	}
	return nil
}

// RequeueFailedAttempt transitions a job from in_progress back to pending
// so it can be retried. Preserves attempt_count and stores the error message.
// Source: JOB_MODEL_V1 §retry.
func (r *Repo) RequeueFailedAttempt(ctx context.Context, id string, errorMessage *string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs
		SET status        = 'pending',
		    claimed_at    = NULL,
		    error_message = $2,
		    updated_at    = NOW()
		WHERE id     = $1
		  AND status = 'in_progress'
	`, id, errorMessage)
	if err != nil {
		return fmt.Errorf("RequeueFailedAttempt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("RequeueFailedAttempt: job %s not found or not in_progress", id)
	}
	return nil
}

// ListStuckInProgressJobs returns jobs that have been in_progress past their
// timeout threshold. Used by the reconciler to recover stuck jobs.
// Source: JOB_MODEL_V1 §stuck_job_recovery.
func (r *Repo) ListStuckInProgressJobs(ctx context.Context) ([]*JobRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, instance_id, volume_id, snapshot_id,
		       COALESCE(image_id, NULL)::VARCHAR(64) AS image_id,
		       job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs
		WHERE status = 'in_progress'
		  AND (
		        -- Instance jobs: 10-minute timeout.
		        (instance_id IS NOT NULL AND claimed_at < NOW() - INTERVAL '10 minutes')
		        -- Volume jobs: 15-minute timeout.
		     OR (volume_id IS NOT NULL AND claimed_at < NOW() - INTERVAL '15 minutes')
		        -- Snapshot jobs: 30-minute timeout (snapshot I/O may be slow).
		     OR (snapshot_id IS NOT NULL AND claimed_at < NOW() - INTERVAL '30 minutes')
		      )
	`)
	if err != nil {
		return nil, fmt.Errorf("ListStuckInProgressJobs: %w", err)
	}
	defer rows.Close()

	var out []*JobRow
	for rows.Next() {
		row := &JobRow{}
		if err := rows.Scan(
			&row.ID, &row.InstanceID, &row.VolumeID, &row.SnapshotID, &row.ImageID, &row.JobType, &row.Status,
			&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
			&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListStuckInProgressJobs scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// HasActivePendingJob reports whether an instance currently has a non-terminal job.
func (r *Repo) HasActivePendingJob(ctx context.Context, instanceID, jobType string) (bool, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM jobs
		WHERE instance_id = $1
		  AND job_type = $2
		  AND status IN ('pending', 'running')
	`, instanceID, jobType).Scan(&count)
	if err != nil {
		if isNoRowsErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("HasActivePendingJob: %w", err)
	}
	return count > 0, nil
}

// RequeueTimedOutJob moves a timed-out running job back to queued.
func (r *Repo) RequeueTimedOutJob(ctx context.Context, jobID string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'queued',
		    updated_at = NOW()
		WHERE id = $1
		  AND status = 'running'
	`, jobID)
	if err != nil {
		return fmt.Errorf("RequeueTimedOutJob: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return nil
}
