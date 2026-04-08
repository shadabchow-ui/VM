package reconciler

// dispatcher.go — Repair job dispatcher.
//
// The dispatcher translates a DriftResult into a repair job record in the DB.
// It never directly calls the runtime or performs destructive operations.
// All repairs go through the job model so workers handle them idempotently.
//
// Idempotency contract:
//   Before inserting a new repair job the dispatcher checks HasActivePendingJob.
//   If a pending/in_progress job of the same type already exists for this
//   instance, the dispatch is skipped. This prevents duplicate repair jobs
//   when the reconciler fires multiple times before the worker processes the first.
//
// Optimistic locking:
//   For drift classes that require an immediate state write (e.g., marking a
//   stuck-provisioning instance as failed) the dispatcher calls
//   UpdateInstanceState with the current version. A 0-row result means another
//   writer already advanced the state — the dispatcher logs and skips.
//
// Source: 03-03-reconciliation-loops §Job-Based Architecture,
//         IMPLEMENTATION_PLAN_V1 §WS-3 (repair action dispatcher),
//         LIFECYCLE_STATE_MACHINE_V1 §7 (optimistic locking),
//         JOB_MODEL_V1 §idempotency.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// Dispatcher creates repair jobs for detected drift.
type Dispatcher struct {
	repo    *db.Repo
	limiter *RateLimiter
	log     *slog.Logger
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(repo *db.Repo, limiter *RateLimiter, log *slog.Logger) *Dispatcher {
	return &Dispatcher{repo: repo, limiter: limiter, log: log}
}

// Dispatch evaluates a DriftResult and takes the appropriate repair action.
// Returns nil when no action was needed (DriftNone or idempotency guard fired).
func (d *Dispatcher) Dispatch(ctx context.Context, inst *db.InstanceRow, drift DriftResult) error {
	if drift.Class == DriftNone {
		return nil
	}

	log := d.log.With(
		"instance_id", inst.ID,
		"vm_state", inst.VMState,
		"drift_class", string(drift.Class),
		"reason", drift.Reason,
	)

	switch drift.Class {

	case DriftStuckProvisioning:
		// Stuck provisioning / stuck requested: transition instance to failed.
		// A repair job is not enqueued here because the instance is already stuck —
		// the correct action is to terminate and let an operator decide on retry.
		// Source: 03-03 §Stuck-Provisioning "Transition db_state to FAILED. Do not retry."
		return d.failInstance(ctx, log, inst,
			fmt.Sprintf("reconciler: %s", drift.Reason))

	case DriftWrongRuntimeState:
		// Enqueue a repair job of the specified type.
		// Source: 03-03 §Wrong-Runtime-State "Enqueue START_INSTANCE / REBOOT_INSTANCE job."
		if drift.RepairJobType == "" {
			log.Warn("dispatcher: DriftWrongRuntimeState with no RepairJobType — skipping")
			return nil
		}
		return d.enqueueRepairJob(ctx, log, inst, drift)

	case DriftMissingRuntimeProcess:
		// Phase 1: mark failed and do not reschedule.
		// Automatic rescheduling to a new host is Phase 2.
		// Source: 03-03 §Missing-Runtime-Process "Transition db_state to FAILED.
		//         Do not reschedule."
		return d.failInstance(ctx, log, inst,
			fmt.Sprintf("reconciler: %s", drift.Reason))

	case DriftOrphanedResource:
		// No host is assigned — the instance record is inconsistent.
		// Mark failed so an operator can investigate.
		// Source: 03-03 §Orphaned-Resource (quarantine + verify pattern;
		//         Phase 1 simplified to immediate failure for no-host case).
		return d.failInstance(ctx, log, inst,
			fmt.Sprintf("reconciler: %s", drift.Reason))

	case DriftJobTimeout:
		// Job timeout is handled by the janitor. The reconciler surfaces it
		// here as an additional detection path (no duplicate job dispatch).
		log.Warn("dispatcher: DriftJobTimeout — janitor handles this; no additional action")
		return nil

	default:
		log.Warn("dispatcher: unrecognised drift class — skipping", "class", string(drift.Class))
		return nil
	}
}

// enqueueRepairJob creates a repair job after checking idempotency and rate limit.
func (d *Dispatcher) enqueueRepairJob(
	ctx context.Context,
	log *slog.Logger,
	inst *db.InstanceRow,
	drift DriftResult,
) error {
	// ── Idempotency guard ─────────────────────────────────────────────────────
	// If a job of this type is already active for this instance, skip.
	// Source: 03-03 §Job-Based Architecture "check if a repair job is already pending".
	active, err := d.repo.HasActivePendingJob(ctx, inst.ID, drift.RepairJobType)
	if err != nil {
		return fmt.Errorf("dispatcher: HasActivePendingJob: %w", err)
	}
	if active {
		log.Info("dispatcher: repair job already active — skipping duplicate dispatch",
			"job_type", drift.RepairJobType)
		return nil
	}

	// ── Rate limit ────────────────────────────────────────────────────────────
	if !d.limiter.Allow(inst.ID) {
		log.Warn("dispatcher: rate limit exceeded — repair job dispatch suppressed",
			"job_type", drift.RepairJobType)
		return nil
	}

	// ── Create repair job ─────────────────────────────────────────────────────
	// Idempotency key scopes uniqueness to instance + drift class so that a
	// re-running reconciler with the same observed state produces no duplicate.
	idempotencyKey := fmt.Sprintf("reconciler:%s:%s", inst.ID, string(drift.Class))
	jobID := idgen.New(idgen.PrefixJob)

	row := &db.JobRow{
		ID:             jobID,
		InstanceID:     inst.ID,
		JobType:        drift.RepairJobType,
		IdempotencyKey: idempotencyKey,
		MaxAttempts:    jobMaxAttempts(drift.RepairJobType),
	}
	if err := d.repo.InsertJob(ctx, row); err != nil {
		return fmt.Errorf("dispatcher: InsertJob: %w", err)
	}

	log.Info("dispatcher: repair job enqueued",
		"job_id", jobID,
		"job_type", drift.RepairJobType,
		"idempotency_key", idempotencyKey)
	return nil
}

// failInstance transitions the instance to failed via optimistic-locked write.
// Source: LIFECYCLE_STATE_MACHINE_V1 §7 (optimistic locking) + §5 (FAIL allowed
// from transitional states).
func (d *Dispatcher) failInstance(ctx context.Context, log *slog.Logger, inst *db.InstanceRow, reason string) error {
	if !isFailableState(inst.VMState) {
		log.Info("dispatcher: instance not in failable state — no transition",
			"vm_state", inst.VMState)
		return nil
	}

	if err := d.repo.UpdateInstanceState(ctx, inst.ID, inst.VMState, "failed", inst.Version); err != nil {
		// 0-rows from UpdateInstanceState means concurrent modification.
		// Log and treat as a safe non-error — another writer already advanced state.
		log.Warn("dispatcher: UpdateInstanceState rejected (stale version or state mismatch) — skipping",
			"error", err)
		return nil
	}

	// Emit failure event.
	_ = d.repo.InsertEvent(ctx, &db.EventRow{
		ID:         idgen.New(idgen.PrefixEvent),
		InstanceID: inst.ID,
		EventType:  db.EventInstanceFailure,
		Message:    reason,
		Actor:      "reconciler",
	})

	log.Warn("dispatcher: instance transitioned to failed",
		"reason", reason)
	return nil
}

// jobMaxAttempts returns the per-type max_attempts from JOB_MODEL_V1 §3.
func jobMaxAttempts(jobType string) int {
	switch jobType {
	case "INSTANCE_CREATE":
		return 3
	case "INSTANCE_DELETE":
		return 5
	case "INSTANCE_START":
		return 5
	case "INSTANCE_STOP":
		return 5
	case "INSTANCE_REBOOT":
		return 5
	default:
		return 3
	}
}
