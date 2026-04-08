package queue

// consumer.go — Message queue consumer using SELECT FOR UPDATE SKIP LOCKED.
//
// Source: IMPLEMENTATION_PLAN_V1 §A5, 03-02-async-job-model-and-idempotency.md.
//
// Claim pattern: BEGIN → SELECT FOR UPDATE SKIP LOCKED WHERE status='pending' LIMIT 1
//   → UPDATE SET status='in_progress', claimed_at=NOW() → COMMIT.
// This guarantees exactly-one delivery under concurrent workers.

import (
	"context"
	"fmt"
	"time"
)

// ClaimedJob is a job record claimed for processing.
type ClaimedJob struct {
	ID             string
	InstanceID     string
	JobType        string
	IdempotencyKey string
	AttemptCount   int
}

// Claimer polls the jobs table and atomically claims a pending job.
type Claimer struct {
	pool TxPool
}

// TxPool extends Pool with transaction support needed for atomic claim.
type TxPool interface {
	Pool
	Begin(ctx context.Context) (Tx, error)
}

// Tx wraps a database transaction.
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// Row wraps pgx.Row.
type Row interface {
	Scan(dest ...any) error
}

// NewClaimer constructs a Claimer.
func NewClaimer(pool TxPool) *Claimer {
	return &Claimer{pool: pool}
}

// ClaimNext atomically claims the next pending job.
// Returns (nil, nil) if no pending job is available.
// Source: JOB_MODEL_V1 §atomic_claim, 03-02 §SELECT FOR UPDATE SKIP LOCKED.
func (c *Claimer) ClaimNext(ctx context.Context) (*ClaimedJob, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("ClaimNext begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	job := &ClaimedJob{}
	err = tx.QueryRow(ctx, `
		SELECT id, instance_id, job_type, idempotency_key, attempt_count
		FROM jobs
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&job.ID, &job.InstanceID, &job.JobType, &job.IdempotencyKey, &job.AttemptCount)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil // no work available
		}
		return nil, fmt.Errorf("ClaimNext query: %w", err)
	}

	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE jobs
		SET status        = 'in_progress',
		    claimed_at    = $2,
		    attempt_count = attempt_count + 1,
		    updated_at    = $2
		WHERE id = $1
	`, job.ID, now)
	if err != nil {
		return nil, fmt.Errorf("ClaimNext update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("ClaimNext commit: %w", err)
	}
	return job, nil
}

// Complete marks a claimed job as completed.
func Complete(ctx context.Context, pool Pool, jobID string) error {
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		UPDATE jobs
		SET status       = 'completed',
		    completed_at = $2,
		    updated_at   = $2
		WHERE id = $1
	`, jobID, now)
	return err
}

// Fail marks a claimed job as failed with an error message.
// If attempt_count >= max_attempts the job transitions to 'dead' (DLQ).
// Source: JOB_MODEL_V1 §retry, IMPLEMENTATION_PLAN_V1 §A5 (DLQ).
func Fail(ctx context.Context, pool Pool, jobID, errMsg string) error {
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		UPDATE jobs
		SET status        = CASE
		                      WHEN attempt_count >= max_attempts THEN 'dead'
		                      ELSE 'pending'
		                    END,
		    error_message = $2,
		    updated_at    = $3
		WHERE id = $1
	`, jobID, errMsg, now)
	return err
}
