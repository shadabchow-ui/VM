package reconciler

// dispatcher_gate_test.go — VM-P3C: tests for RolloutGate integration in Dispatcher.
//
// Tests verify:
//   - When gate is nil (default), repair job dispatch proceeds normally
//   - When gate is paused, enqueueRepairJob is suppressed
//   - When gate is paused, failInstance is NOT suppressed (state corrections safe)
//   - When gate is resumed, dispatch proceeds normally again
//
// Uses the existing dispatchFakePool from dispatcher_test.go (same package).
// Source: VM_PHASE_ROADMAP §9 "bounded rollout controls",
//         dispatcher.go §"RolloutGate integration".

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// dispatchCtx returns a background context for dispatcher tests.
func dispatchCtx() context.Context { return context.Background() }

// dispatchLog returns a silent logger for dispatcher tests.
func dispatchLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newDispatcherWithGate constructs a Dispatcher wired with the given gate.
func newDispatcherWithGate(pool *dispatchFakePool, gate *RolloutGate) *Dispatcher {
	repo := db.New(pool)
	limiter := NewRateLimiter()
	d := NewDispatcher(repo, limiter, dispatchLog())
	if gate != nil {
		d.SetGate(gate)
	}
	return d
}

// makeDriftedInstForGate returns a WrongRuntimeState drift so we exercise enqueueRepairJob.
func makeDriftedInstForGate(id string) *db.InstanceRow {
	return &db.InstanceRow{
		ID:        id,
		VMState:   "stopping",
		Version:   1,
		UpdatedAt: time.Now().Add(-20 * time.Minute),
	}
}

// makeDriftedInstProvisioning returns a stuck-provisioning instance so we exercise failInstance.
func makeDriftedInstProvisioning(id string) *db.InstanceRow {
	return &db.InstanceRow{
		ID:        id,
		VMState:   "provisioning",
		Version:   1,
		UpdatedAt: time.Now().Add(-20 * time.Minute),
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestDispatcher_NilGate_DispatchProceeds verifies that a nil gate (default)
// allows enqueueRepairJob to proceed normally.
func TestDispatcher_NilGate_DispatchProceeds(t *testing.T) {
	pool := newDispatchFakePool()
	d := newDispatcherWithGate(pool, nil) // no gate

	inst := makeDriftedInstForGate("inst-gate-001")
	drift := DriftResult{
		Class:         DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck in stopping",
	}

	if err := d.Dispatch(dispatchCtx(), inst, drift); err != nil {
		t.Fatalf("Dispatch with nil gate: %v", err)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) == 0 {
		t.Error("nil gate: repair job must be inserted when gate is absent")
	}
}

// TestDispatcher_GatePaused_SuppressesRepairJobEnqueue verifies that pausing the
// gate suppresses enqueueRepairJob for WrongRuntimeState drift.
// Source: dispatcher.go §enqueueRepairJob "rollout gate suppresses new repair job creation".
func TestDispatcher_GatePaused_SuppressesRepairJobEnqueue(t *testing.T) {
	pool := newDispatchFakePool()
	gate := NewRolloutGate()
	gate.Pause("upgrading worker binary")
	d := newDispatcherWithGate(pool, gate)

	inst := makeDriftedInstForGate("inst-gate-002")
	drift := DriftResult{
		Class:         DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck in stopping",
	}

	if err := d.Dispatch(dispatchCtx(), inst, drift); err != nil {
		t.Fatalf("Dispatch with paused gate: %v", err)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) != 0 {
		t.Errorf("paused gate: expected 0 job inserts, got %d", len(pool.insertJobCalls))
	}
}

// TestDispatcher_GatePaused_DoesNotSuppressFailInstance verifies that failInstance
// (for DriftStuckProvisioning) is NOT gated — state corrections are safe during rollouts.
// Source: dispatcher.go §Dispatch "failInstance is NOT gated by RolloutGate".
func TestDispatcher_GatePaused_DoesNotSuppressFailInstance(t *testing.T) {
	pool := newDispatchFakePool()
	gate := NewRolloutGate()
	gate.Pause("db migration running")
	d := newDispatcherWithGate(pool, gate)

	inst := makeDriftedInstProvisioning("inst-gate-003")
	drift := DriftResult{
		Class:  DriftStuckProvisioning,
		Reason: "stuck in provisioning for > 15 min",
	}

	if err := d.Dispatch(dispatchCtx(), inst, drift); err != nil {
		t.Fatalf("Dispatch DriftStuckProvisioning with paused gate: %v", err)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	// failInstance calls UpdateInstanceState — must have executed despite gate.
	if len(pool.updateStateCalls) == 0 {
		t.Error("paused gate must NOT suppress failInstance (UpdateInstanceState must be called)")
	}
	// And must NOT insert a repair job.
	if len(pool.insertJobCalls) != 0 {
		t.Errorf("paused gate must not insert jobs for DriftStuckProvisioning, got %d", len(pool.insertJobCalls))
	}
}

// TestDispatcher_GateResumed_DispatchResumes verifies that repair jobs are
// created again after Resume() is called.
func TestDispatcher_GateResumed_DispatchResumes(t *testing.T) {
	pool := newDispatchFakePool()
	gate := NewRolloutGate()
	gate.Pause("paused for rollout")
	d := newDispatcherWithGate(pool, gate)

	inst := makeDriftedInstForGate("inst-gate-004")
	drift := DriftResult{
		Class:         DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck in stopping",
	}

	// While paused — no insert.
	if err := d.Dispatch(dispatchCtx(), inst, drift); err != nil {
		t.Fatalf("Dispatch while paused: %v", err)
	}
	pool.mu.Lock()
	if len(pool.insertJobCalls) != 0 {
		pool.mu.Unlock()
		t.Fatalf("paused: expected 0 inserts, got %d", len(pool.insertJobCalls))
	}
	pool.mu.Unlock()

	// Resume and dispatch again — job should be inserted now.
	gate.Resume()
	if err := d.Dispatch(dispatchCtx(), inst, drift); err != nil {
		t.Fatalf("Dispatch after resume: %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) == 0 {
		t.Error("after resume: repair job must be inserted")
	}
}

// TestDispatcher_SetGate_NilGate_IsNoop verifies SetGate(nil) does not
// introduce any dispatch suppression.
func TestDispatcher_SetGate_NilGate_IsNoop(t *testing.T) {
	pool := newDispatchFakePool()
	gate := NewRolloutGate()
	d := newDispatcherWithGate(pool, gate)
	d.SetGate(nil) // clear the gate

	inst := makeDriftedInstForGate("inst-gate-005")
	drift := DriftResult{
		Class:         DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "test",
	}
	if err := d.Dispatch(dispatchCtx(), inst, drift); err != nil {
		t.Fatalf("Dispatch with nil gate after SetGate(nil): %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) == 0 {
		t.Error("SetGate(nil): repair job must not be suppressed")
	}
}
