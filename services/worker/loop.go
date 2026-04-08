package main

// loop.go — Worker poll loop: poll → claim → route → execute → update.
//
// Source: IMPLEMENTATION_PLAN_V1 §21, 03-02-async-job-model-and-idempotency.md.
//
// The worker is a durable background service that:
//  1. Polls the jobs table for pending work (FIFO, SELECT FOR UPDATE SKIP LOCKED).
//  2. Atomically claims a job (status pending → in_progress).
//  3. Routes by job_type to the appropriate handler.
//  4. On success: marks the job completed.
//  5. On failure: increments attempt_count; if max_attempts reached, marks dead.
//
// Idempotency guarantee: each handler is responsible for idempotent execution.
// The job model guarantees at-least-once delivery; handlers must tolerate replay.
//
// Source: JOB_MODEL_V1 §atomic_claim.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/services/worker/handlers"
)

const (
	// pollInterval is how long the worker sleeps when no job is available.
	pollInterval = 2 * time.Second
	// jobTimeout is the maximum time a single job execution may take.
	jobTimeout = 10 * time.Minute
)

// WorkerLoop is the main job processing loop.
type WorkerLoop struct {
	repo     *db.Repo
	sqlDB    *sql.DB
	dispatch map[string]handlers.Handler
	log      *slog.Logger
}

// NewWorkerLoop constructs a WorkerLoop with the given handler dispatch table.
func NewWorkerLoop(repo *db.Repo, sqlDB *sql.DB, dispatch map[string]handlers.Handler, log *slog.Logger) *WorkerLoop {
	return &WorkerLoop{repo: repo, sqlDB: sqlDB, dispatch: dispatch, log: log}
}

// Run starts the poll loop. Blocks until ctx is cancelled.
// Intended to run in its own goroutine.
func (w *WorkerLoop) Run(ctx context.Context) {
	w.log.Info("worker loop started")
	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker loop stopped")
			return
		default:
		}

		claimed, err := w.claimNext(ctx)
		if err != nil {
			w.log.Error("claim error — retrying", "error", err)
			sleep(ctx, pollInterval)
			continue
		}
		if claimed == nil {
			// No pending work.
			sleep(ctx, pollInterval)
			continue
		}

		w.execute(ctx, claimed)
	}
}

// claimNext atomically claims the next pending job.
// Returns nil, nil when there are no pending jobs.
// Uses a BEGIN/SELECT FOR UPDATE SKIP LOCKED/UPDATE/COMMIT transaction.
func (w *WorkerLoop) claimNext(ctx context.Context) (*db.JobRow, error) {
	tx, err := w.sqlDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("claimNext: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// SELECT FOR UPDATE SKIP LOCKED — skip jobs locked by another worker.
	var job db.JobRow
	err = tx.QueryRowContext(ctx, `
		SELECT id, instance_id, job_type, status,
		       idempotency_key, attempt_count, max_attempts,
		       error_message, created_at, updated_at, claimed_at, completed_at
		FROM jobs
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(
		&job.ID, &job.InstanceID, &job.JobType, &job.Status,
		&job.IdempotencyKey, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt, &job.UpdatedAt, &job.ClaimedAt, &job.CompletedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claimNext: select: %w", err)
	}

	// Atomically transition to in_progress.
	_, err = tx.ExecContext(ctx, `
		UPDATE jobs
		SET status        = 'in_progress',
		    attempt_count = attempt_count + 1,
		    claimed_at    = NOW(),
		    updated_at    = NOW()
		WHERE id = $1
	`, job.ID)
	if err != nil {
		return nil, fmt.Errorf("claimNext: update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claimNext: commit: %w", err)
	}

	job.AttemptCount++ // reflect the increment
	return &job, nil
}

// execute runs a job, then updates its final status.
func (w *WorkerLoop) execute(ctx context.Context, job *db.JobRow) {
	log := w.log.With("job_id", job.ID, "job_type", job.JobType, "instance_id", job.InstanceID, "attempt", job.AttemptCount)
	log.Info("executing job")

	handler, ok := w.dispatch[job.JobType]
	if !ok {
		errMsg := fmt.Sprintf("no handler registered for job type %q — marking dead", job.JobType)
		log.Error(errMsg)
		// Unknown type is not retriable: no amount of attempts will fix it.
		// Mark dead immediately regardless of attempt count.
		if err := w.repo.UpdateJobStatus(ctx, job.ID, "dead", &errMsg); err != nil {
			log.Error("failed to mark unroutable job dead", "error", err)
		}
		return
	}

	jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	execErr := handler.Execute(jobCtx, job)
	w.finaliseJob(ctx, job, execErr)
}

// finaliseJob marks the job completed or failed/dead.
func (w *WorkerLoop) finaliseJob(ctx context.Context, job *db.JobRow, execErr error) {
	log := w.log.With("job_id", job.ID)

	if execErr == nil {
		if err := w.repo.UpdateJobStatus(ctx, job.ID, "completed", nil); err != nil {
			log.Error("failed to mark job completed", "error", err)
		} else {
			log.Info("job completed")
		}
		return
	}

	log.Warn("job failed", "error", execErr)
	errMsg := execErr.Error()

	// Determine terminal status.
	status := "failed"
	if job.AttemptCount >= job.MaxAttempts {
		status = "dead"
		log.Error("job exhausted max attempts — moving to dead letter",
			"attempt_count", job.AttemptCount,
			"max_attempts", job.MaxAttempts,
		)
	}

	if err := w.repo.UpdateJobStatus(ctx, job.ID, status, &errMsg); err != nil {
		log.Error("failed to update job status after failure", "error", err)
	}
}

// sleep waits for d or until ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
