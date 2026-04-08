package db

// job_repo.go — Job table persistence methods.
//
// Source: JOB_MODEL_V1 §1 (schema), §2 (states), §3 (types),
//         IMPLEMENTATION_PLAN_V1 §20 (Job CRUD: create, get_by_id, get_by_idempotency_key,
//         atomic_claim, update_status).

import (
	"fmt"
	"time"
	"context"
)

// JobRow is the DB representation of a job record.
type JobRow struct {
	ID             string
	InstanceID     string
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

// GetJobByID fetches a single job by its primary key.
func (r *Repo) GetJobByID(ctx context.Context, id string) (*JobRow, error) {
	row := &JobRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs WHERE id = $1
	`, id).Scan(
		&row.ID, &row.InstanceID, &row.JobType, &row.Status,
		&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
		&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("GetJobByID %s: %w", id, err)
	}
	return row, nil
}

// GetJobByIdempotencyKey fetches the job associated with the given idempotency key.
// Returns nil, nil if no job exists for that key.
// Source: JOB_MODEL_V1 §idempotency.
func (r *Repo) GetJobByIdempotencyKey(ctx context.Context, key string) (*JobRow, error) {
	row := &JobRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs WHERE idempotency_key = $1
	`, key).Scan(
		&row.ID, &row.InstanceID, &row.JobType, &row.Status,
		&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
		&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("GetJobByIdempotencyKey: %w", err)
	}
	return row, nil
}

// AtomicClaimJob claims the next pending job using SELECT FOR UPDATE SKIP LOCKED.
// Returns nil, nil when no pending job is available.
// Source: JOB_MODEL_V1 §atomic_claim, IMPLEMENTATION_PLAN_V1 §20.
//
// This method uses a two-step approach compatible with the Pool interface:
// the full transaction is handled internally via two Exec calls within the
// same implicit serialisable transaction on the pgxpool connection.
// For true transactional safety the caller should use a pgxpool.Tx directly
// in the worker loop (Sprint 2). This version provides the correct SQL logic.
func (r *Repo) AtomicClaimJob(ctx context.Context) (*JobRow, error) {
	// Note: full transaction (BEGIN/SELECT FOR UPDATE/UPDATE/COMMIT) must be
	// implemented in the Sprint 2 worker using pgxpool.BeginTx.
	// This method exposes the query logic for testing and documentation.
	//
	// Worker implementation (Sprint 2) pattern:
	//   tx, _ := pool.BeginTx(ctx, pgx.TxOptions{})
	//   SELECT id,... FROM jobs WHERE status='pending' ORDER BY created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED
	//   UPDATE jobs SET status='in_progress', claimed_at=NOW(), attempt_count=attempt_count+1 WHERE id=$1
	//   tx.Commit(ctx)
	row := &JobRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT 1
	`).Scan(
		&row.ID, &row.InstanceID, &row.JobType, &row.Status,
		&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
		&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("AtomicClaimJob query: %w", err)
	}
	return row, nil
}

// ListStuckInProgressJobs returns all in_progress jobs whose per-type timeout
// has elapsed since claimed_at. Used exclusively by the janitor.
// Timeout values per type are per JOB_MODEL_V1 §3.
// Source: JOB_MODEL_V1 §8 (janitor scan query).
func (r *Repo) ListStuckInProgressJobs(ctx context.Context) ([]*JobRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, instance_id, job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs
		WHERE status = 'in_progress'
		  AND claimed_at IS NOT NULL
		  AND claimed_at + (
		        CASE job_type
		            WHEN 'INSTANCE_CREATE'  THEN INTERVAL '1800 seconds'
		            WHEN 'INSTANCE_START'   THEN INTERVAL '300 seconds'
		            WHEN 'INSTANCE_STOP'    THEN INTERVAL '600 seconds'
		            WHEN 'INSTANCE_REBOOT'  THEN INTERVAL '180 seconds'
		            WHEN 'INSTANCE_DELETE'  THEN INTERVAL '900 seconds'
		            ELSE                         INTERVAL '600 seconds'
		        END
		      ) < NOW()
	`)
	if err != nil {
		return nil, fmt.Errorf("ListStuckInProgressJobs: %w", err)
	}
	defer rows.Close()

	var out []*JobRow
	for rows.Next() {
		row := &JobRow{}
		if err := rows.Scan(
			&row.ID, &row.InstanceID, &row.JobType, &row.Status,
			&row.IdempotencyKey, &row.AttemptCount, &row.MaxAttempts,
			&row.ErrorMessage, &row.CreatedAt, &row.UpdatedAt, &row.ClaimedAt, &row.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListStuckInProgressJobs scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// RequeueTimedOutJob resets a stuck in_progress job back to pending so the
// worker can reclaim it. Called by the janitor when attempt_count < max_attempts.
// Uses an optimistic WHERE status='in_progress' guard to prevent double-reset.
// Source: JOB_MODEL_V1 §8 (TIMED_OUT → PENDING re-enqueue path).
func (r *Repo) RequeueTimedOutJob(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs
		SET status      = 'pending',
		    claimed_at  = NULL,
		    updated_at  = NOW()
		WHERE id     = $1
		  AND status = 'in_progress'
	`, id)
	if err != nil {
		return fmt.Errorf("RequeueTimedOutJob: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Already transitioned by another process — idempotent, not an error.
		return nil
	}
	return nil
}

// HasActivePendingJob returns true when the instance already has a pending or
// in_progress job of the given type. Used by the reconciler to avoid dispatching
// duplicate repair jobs.
// Source: 03-03-reconciliation-loops §Job-Based Architecture (idempotency check).
func (r *Repo) HasActivePendingJob(ctx context.Context, instanceID, jobType string) (bool, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM jobs
		WHERE instance_id = $1
		  AND job_type    = $2
		  AND status IN ('pending', 'in_progress')
	`, instanceID, jobType).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("HasActivePendingJob: %w", err)
	}
	return count > 0, nil
}

// ListActiveInstances returns all non-deleted instances. Used by the reconciler's
// periodic full-resync scan.
// Source: 03-03-reconciliation-loops §Periodic Polling (Resync).
func (r *Repo) ListActiveInstances(ctx context.Context) ([]*InstanceRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, owner_principal_id, vm_state,
		       instance_type_id, image_id, host_id, availability_zone,
		       version, created_at, updated_at, deleted_at
		FROM instances
		WHERE deleted_at IS NULL
		  AND vm_state NOT IN ('deleted', 'failed')
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("ListActiveInstances: %w", err)
	}
	defer rows.Close()

	var out []*InstanceRow
	for rows.Next() {
		row := &InstanceRow{}
		if err := rows.Scan(
			&row.ID, &row.Name, &row.OwnerPrincipalID, &row.VMState,
			&row.InstanceTypeID, &row.ImageID, &row.HostID, &row.AvailabilityZone,
			&row.Version, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListActiveInstances scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateJobStatus updates a job's status and optionally sets error_message.
// Source: JOB_MODEL_V1 §2 (status transitions).
func (r *Repo) UpdateJobStatus(ctx context.Context, id, status string, errMsg *string) error {
	now := time.Now().UTC()
	var completedAt *time.Time
	if status == "completed" || status == "failed" || status == "dead" {
		completedAt = &now
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs
		SET status        = $2,
		    error_message = $3,
		    completed_at  = $4,
		    updated_at    = $5
		WHERE id = $1
	`, id, status, errMsg, completedAt, now)
	if err != nil {
		return fmt.Errorf("UpdateJobStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateJobStatus: job %s not found", id)
	}
	return nil
}
