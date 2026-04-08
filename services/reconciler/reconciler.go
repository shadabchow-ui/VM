package reconciler

// reconciler.go — Reconciliation engine: hybrid trigger model.
//
// Architecture:
//   - Event-driven trigger: external callers call Enqueue(instanceID) when they
//     observe a state change (API mutations, worker completions, failure reports).
//   - Periodic resync: RunPeriodicResync scans all active instances every 5 minutes
//     and enqueues each for reconciliation. This is the backstop for missed events.
//   - Work queue: a buffered channel of instance IDs deduplicates rapid re-triggers.
//   - Reconcile loop: RunWorkers drains the channel, one goroutine per worker.
//
// For Phase 1, this is a single-process, single-reconciler deployment.
// The work channel provides local-process deduplication; distributed lease
// management is deferred to Phase 2.
//
// Source: 03-03-reconciliation-loops-and-state-authority.md §Reconciliation Trigger Model,
//         IMPLEMENTATION_PLAN_V1 §WS-3 (outputs: hybrid trigger model, 5-min resync),
//         R-07 (hybrid trigger is non-negotiable),
//         12-02-implementation-sequence §M4 gate.

import (
	"context"
	"log/slog"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

const (
	// resyncInterval is how often the full reconciler resync runs.
	// Source: IMPLEMENTATION_PLAN_V1 §R-07, 03-03 §Periodic Polling "N=5 minutes".
	resyncInterval = 5 * time.Minute

	// workQueueDepth is the buffer size of the instance-ID work queue.
	// Deep enough to absorb a burst from the periodic resync without blocking.
	workQueueDepth = 512
)

// Reconciler is the Phase 1 hybrid-trigger reconciliation engine.
type Reconciler struct {
	repo       *db.Repo
	dispatcher *Dispatcher
	log        *slog.Logger
	work       chan string // buffered channel of instance IDs to reconcile
}

// NewReconciler constructs a Reconciler. The caller must also start the janitor
// separately via NewJanitor.
func NewReconciler(repo *db.Repo, log *slog.Logger) *Reconciler {
	limiter := NewRateLimiter()
	return &Reconciler{
		repo:       repo,
		dispatcher: NewDispatcher(repo, limiter, log),
		log:        log,
		work:       make(chan string, workQueueDepth),
	}
}

// Enqueue submits an instance ID for reconciliation.
// Non-blocking: if the queue is full the enqueue is dropped with a warning.
// Correctness is preserved because the periodic resync will catch it on the
// next cycle.
// Source: 03-03 §Event-Driven Triggers.
func (r *Reconciler) Enqueue(instanceID string) {
	select {
	case r.work <- instanceID:
	default:
		r.log.Warn("reconciler: work queue full — dropping enqueue",
			"instance_id", instanceID)
	}
}

// RunPeriodicResync starts the 5-minute periodic full-resync loop.
// Blocks until ctx is cancelled. Run in its own goroutine.
// Source: IMPLEMENTATION_PLAN_V1 §R-07, 03-03 §Periodic Polling.
func (r *Reconciler) RunPeriodicResync(ctx context.Context) {
	r.log.Info("reconciler: periodic resync started", "interval", resyncInterval)
	for {
		select {
		case <-ctx.Done():
			r.log.Info("reconciler: periodic resync stopped")
			return
		case <-time.After(resyncInterval):
		}
		r.resync(ctx)
	}
}

// resync scans all active instances and enqueues each for reconciliation.
// Source: 03-03 §Periodic Polling (step 2–3).
func (r *Reconciler) resync(ctx context.Context) {
	r.log.Info("reconciler: starting full resync")
	instances, err := r.repo.ListActiveInstances(ctx)
	if err != nil {
		r.log.Error("reconciler: resync ListActiveInstances failed", "error", err)
		return
	}
	for _, inst := range instances {
		r.Enqueue(inst.ID)
	}
	r.log.Info("reconciler: resync enqueued instances", "count", len(instances))
}

// RunWorkers drains the work channel, calling reconcileOne for each instance.
// Blocks until ctx is cancelled. Run in its own goroutine.
func (r *Reconciler) RunWorkers(ctx context.Context) {
	r.log.Info("reconciler: worker loop started")
	for {
		select {
		case <-ctx.Done():
			r.log.Info("reconciler: worker loop stopped")
			return
		case instanceID, ok := <-r.work:
			if !ok {
				return
			}
			r.reconcileOne(ctx, instanceID)
		}
	}
}

// reconcileOne runs the reconciliation loop for a single instance.
// It reads the instance from DB, classifies drift, and dispatches a repair.
// Source: 03-03 §pseudocode reconcile(instanceID).
func (r *Reconciler) reconcileOne(ctx context.Context, instanceID string) {
	log := r.log.With("instance_id", instanceID)

	inst, err := r.repo.GetInstanceByID(ctx, instanceID)
	if err != nil {
		// Instance may have been deleted — not an error worth surfacing loudly.
		log.Info("reconciler: instance not found — skipping", "error", err)
		return
	}

	drift := ClassifyDrift(inst, time.Now())

	if drift.Class == DriftNone {
		return
	}

	log.Info("reconciler: drift detected",
		"drift_class", string(drift.Class),
		"reason", drift.Reason)

	if err := r.dispatcher.Dispatch(ctx, inst, drift); err != nil {
		log.Error("reconciler: dispatch failed", "error", err,
			"drift_class", string(drift.Class))
	}
}
