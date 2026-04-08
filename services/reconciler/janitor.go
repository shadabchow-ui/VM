package reconciler

// janitor.go — Job timeout janitor.
//
// Detects stuck in_progress jobs and applies the JOB_MODEL_V1 §8 recovery logic:
//
//   IF attempt_count < max_attempts:
//       reset job status → pending (worker reclaims on next poll)
//   ELSE:
//       mark job dead + transition instance → failed
//
// The janitor runs on a fixed 60-second interval. It is idempotent: a repeat
// run with no new stuck jobs is a safe no-op.
//
// Source: JOB_MODEL_V1 §8 (janitor scan + recovery rules),
//         IMPLEMENTATION_PLAN_V1 §WS-3 (janitor output),
//         11-03-failure-surface-and-recovery-map.md §Worker Crashes.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

const janitorInterval = 60 * time.Second

// Janitor scans for stuck in_progress jobs and applies timeout recovery.
type Janitor struct {
	repo *db.Repo
	log  *slog.Logger
}

// NewJanitor constructs a Janitor.
func NewJanitor(repo *db.Repo, log *slog.Logger) *Janitor {
	return &Janitor{repo: repo, log: log}
}

// Run starts the janitor loop. Blocks until ctx is cancelled.
// Intended to run in its own goroutine.
func (j *Janitor) Run(ctx context.Context) {
	j.log.Info("janitor started")
	for {
		select {
		case <-ctx.Done():
			j.log.Info("janitor stopped")
			return
		case <-time.After(janitorInterval):
		}
		j.sweep(ctx)
	}
}

// Sweep performs a single janitor scan. Safe to call directly in tests.
func (j *Janitor) Sweep(ctx context.Context) {
	j.sweep(ctx)
}

func (j *Janitor) sweep(ctx context.Context) {
	stuck, err := j.repo.ListStuckInProgressJobs(ctx)
	if err != nil {
		j.log.Error("janitor: ListStuckInProgressJobs failed", "error", err)
		return
	}
	if len(stuck) == 0 {
		return
	}
	j.log.Info("janitor: found stuck jobs", "count", len(stuck))

	for _, job := range stuck {
		j.handleStuck(ctx, job)
	}
}

// handleStuck applies the JOB_MODEL_V1 §8 branching logic to one stuck job.
func (j *Janitor) handleStuck(ctx context.Context, job *db.JobRow) {
	log := j.log.With("job_id", job.ID, "instance_id", job.InstanceID,
		"job_type", job.JobType, "attempt_count", job.AttemptCount,
		"max_attempts", job.MaxAttempts)

	if job.AttemptCount < job.MaxAttempts {
		// Below retry limit — reset to pending so the worker reclaims it.
		// Source: JOB_MODEL_V1 §8 "Reset to PENDING, re-enqueue".
		if err := j.repo.RequeueTimedOutJob(ctx, job.ID); err != nil {
			log.Error("janitor: failed to requeue timed-out job", "error", err)
			return
		}
		log.Warn("janitor: timed-out job reset to pending for retry")
		return
	}

	// Exhausted max_attempts — mark dead and fail the instance.
	// Source: JOB_MODEL_V1 §8 "UPDATE jobs SET status='FAILED' …
	//         UPDATE instances SET vm_state='FAILED' WHERE id=$instance_id".
	errMsg := fmt.Sprintf("job timed out after %d attempts", job.AttemptCount)
	if err := j.repo.UpdateJobStatus(ctx, job.ID, "dead", &errMsg); err != nil {
		log.Error("janitor: failed to mark job dead", "error", err)
		return
	}
	log.Error("janitor: job exhausted max attempts — marked dead",
		"error_message", errMsg)

	// Transition instance to failed. We do not know the exact current version,
	// so we read the instance first to get the current version before writing.
	// The UpdateInstanceState optimistic lock will reject a stale write safely.
	inst, err := j.repo.GetInstanceByID(ctx, job.InstanceID)
	if err != nil {
		log.Error("janitor: could not load instance to fail it", "error", err)
		return
	}
	// Only fail if instance is in a transitional state. If it is already
	// failed, deleted, or running, leave it alone.
	// Source: LIFECYCLE_STATE_MACHINE_V1 §5 (FAIL allowed from transitional states).
	if !isFailableState(inst.VMState) {
		log.Info("janitor: instance not in failable state — skipping fail transition",
			"vm_state", inst.VMState)
		return
	}
	if err := j.repo.UpdateInstanceState(ctx, inst.ID, inst.VMState, "failed", inst.Version); err != nil {
		log.Error("janitor: failed to transition instance to failed", "error", err)
		return
	}
	// Emit failure event.
	_ = j.repo.InsertEvent(ctx, &db.EventRow{
		ID:         idgen.New(idgen.PrefixEvent),
		InstanceID: inst.ID,
		EventType:  db.EventInstanceFailure,
		Message:    fmt.Sprintf("Job %s timed out after %d attempts", job.JobType, job.AttemptCount),
		Actor:      "janitor",
	})
	log.Error("janitor: instance transitioned to failed due to job timeout",
		"vm_state_was", inst.VMState)
}

// isFailableState returns true for the transitional states that the lifecycle
// contract permits a FAIL transition from.
// Source: LIFECYCLE_STATE_MACHINE_V1 §5 (Failure Transitions).
func isFailableState(state string) bool {
	switch state {
	case "requested", "provisioning", "stopping", "rebooting", "deleting":
		return true
	}
	return false
}
