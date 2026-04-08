package queue

// producer.go — Message queue producer backed by PostgreSQL.
//
// Source: IMPLEMENTATION_PLAN_V1 §A5 (message queue producer/consumer with DLQ),
//         03-02-async-job-model-and-idempotency.md.
//
// Design: jobs table IS the queue. Producer inserts a row with status='pending'.
// Consumer uses SELECT FOR UPDATE SKIP LOCKED to claim jobs atomically.
// This avoids an external queue dependency for Phase 1.
//
// On the real machine: pool is *pgxpool.Pool satisfying the db.Pool interface.

import (
	"context"
	"fmt"
	"time"
)

// Enqueuer inserts job records into the jobs table for the worker to process.
// Source: JOB_MODEL_V1 §1 (job schema), 03-02 §queue design.
type Enqueuer struct {
	pool Pool
}

// Pool is the minimal interface needed by the queue package.
// Satisfied by *pgxpool.Pool and by db.Pool.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
}

// CommandTag wraps pgconn.CommandTag.
type CommandTag interface {
	RowsAffected() int64
}

// NewEnqueuer constructs an Enqueuer.
func NewEnqueuer(pool Pool) *Enqueuer {
	return &Enqueuer{pool: pool}
}

// EnqueueJob inserts a new job with status='pending'.
// idempotencyKey ensures the same logical operation is not queued twice.
// Source: JOB_MODEL_V1 §1, 03-02-async-job-model §idempotency.
func (e *Enqueuer) EnqueueJob(ctx context.Context, jobID, instanceID, jobType, idempotencyKey string) error {
	_, err := e.pool.Exec(ctx, `
		INSERT INTO jobs (
			id, instance_id, job_type, status,
			idempotency_key, attempt_count, max_attempts,
			created_at, updated_at
		) VALUES ($1, $2, $3, 'pending', $4, 0, 3, $5, $5)
		ON CONFLICT (idempotency_key) DO NOTHING
	`, jobID, instanceID, jobType, idempotencyKey, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("EnqueueJob: %w", err)
	}
	return nil
}
