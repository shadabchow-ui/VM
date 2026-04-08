package queue

// dlq.go — Dead letter queue consumer.
//
// Source: IMPLEMENTATION_PLAN_V1 §A5 (DLQ consumer: log poison message, alert, do not re-queue),
//         03-02-async-job-model-and-idempotency.md §failure handling.
//
// Jobs land in the DLQ (status='dead') when attempt_count >= max_attempts.
// The DLQ consumer logs the failure, emits a structured alert, and does NOT re-queue.

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DeadJob is a job that exhausted all retry attempts.
type DeadJob struct {
	ID           string
	InstanceID   string
	JobType      string
	ErrorMessage string
	AttemptCount int
	UpdatedAt    time.Time
}

// DLQRows is the minimal interface for iterating dead job rows.
type DLQRows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// QueryPool extends Pool with the Query method needed by the DLQ consumer.
type QueryPool interface {
	Pool
	Query(ctx context.Context, sql string, args ...any) (DLQRows, error)
}

// DLQConsumer scans for dead jobs, logs them, and marks them acknowledged.
// Source: IMPLEMENTATION_PLAN_V1 §22 (DLQ consumer: log + do not re-queue).
type DLQConsumer struct {
	pool QueryPool
	log  *slog.Logger
}

// NewDLQConsumer constructs a DLQConsumer.
func NewDLQConsumer(pool QueryPool, log *slog.Logger) *DLQConsumer {
	return &DLQConsumer{pool: pool, log: log}
}

// DrainDead reads all status='dead' jobs, logs each one, and marks them acknowledged.
// Called periodically (e.g. every 5 minutes) to drain the DLQ.
func (d *DLQConsumer) DrainDead(ctx context.Context) error {
	rows, err := d.pool.Query(ctx, `
		SELECT id, instance_id, job_type, error_message, attempt_count, updated_at
		FROM jobs
		WHERE status = 'dead'
		ORDER BY updated_at ASC
		LIMIT 100
	`)
	if err != nil {
		return fmt.Errorf("DLQ drain query: %w", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		j := &DeadJob{}
		if err := rows.Scan(&j.ID, &j.InstanceID, &j.JobType, &j.ErrorMessage, &j.AttemptCount, &j.UpdatedAt); err != nil {
			return fmt.Errorf("DLQ scan: %w", err)
		}
		// Log at error level — dead jobs require operator attention.
		// Source: 11-01-logging-strategy §dead-job-alert.
		d.log.Error("DLQ: job exhausted all retries — manual intervention required",
			"job_id", j.ID,
			"instance_id", j.InstanceID,
			"job_type", j.JobType,
			"attempt_count", j.AttemptCount,
			"last_error", j.ErrorMessage,
			"dead_at", j.UpdatedAt,
		)
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("DLQ rows: %w", err)
	}
	if count > 0 {
		d.log.Warn("DLQ drained", "dead_jobs_logged", count)
	}
	return nil
}
