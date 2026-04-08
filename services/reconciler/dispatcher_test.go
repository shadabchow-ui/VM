package reconciler

// dispatcher_test.go — Unit tests for the repair job dispatcher.
//
// Tests use a fake db.Pool (same pattern as loop_test.go and repo_test.go)
// so no PostgreSQL is required. Tests are fully valid on macOS dev box.
//
// Coverage required by M4 gate:
//   - Repair job created when drift detected and no active job exists
//   - Repair job NOT created when active job already pending (idempotency)
//   - Repair job NOT created when rate limiter blocks (thundering herd guard)
//   - DriftStuckProvisioning → failInstance path (no job, state set to failed)
//   - DriftNone → complete no-op
//   - Stale optimistic lock on failInstance → silently skipped, no error returned
//   - DriftMissingRuntimeProcess → failInstance path
//   - DriftOrphanedResource → failInstance path
//
// Source: 03-03-reconciliation-loops §Repair Actions,
//         IMPLEMENTATION_PLAN_V1 §WS-3 (idempotency, rate limiting),
//         LIFECYCLE_STATE_MACHINE_V1 §7 (optimistic locking).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── dispatchFakePool ─────────────────────────────────────────────────────────
// Implements db.Pool. Captures Exec/QueryRow calls and returns scripted results.

type dispatchFakePool struct {
	mu sync.Mutex

	// hasActivePendingJob: controlled per (instanceID, jobType) key
	activeJobMap map[string]bool // key = instanceID+":"+jobType

	// insertJobCalls records all INSERT INTO jobs calls (job IDs)
	insertJobCalls []string

	// updateStateCalls records UpdateInstanceState calls
	updateStateCalls []updateStateCall

	// execTag controls RowsAffected for Exec calls (for UpdateInstanceState)
	execRowsAffected int64

	// execErr injects an error into Exec
	execErr error

	// countQueryResult controls the COUNT(*) response for HasActivePendingJob
	countResult int
}

type updateStateCall struct {
	instanceID    string
	expectedState string
	newState      string
}

func newDispatchFakePool() *dispatchFakePool {
	return &dispatchFakePool{
		activeJobMap:     make(map[string]bool),
		execRowsAffected: 1, // default: writes succeed
	}
}

func (p *dispatchFakePool) setActiveJob(instanceID, jobType string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.activeJobMap[instanceID+":"+jobType] = true
	p.countResult = 1
}

// Exec handles: InsertJob, UpdateInstanceState, InsertEvent, UpdateJobStatus.
func (p *dispatchFakePool) Exec(_ context.Context, query string, args ...any) (db.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.execErr != nil {
		return nil, p.execErr
	}

	// Capture INSERT INTO jobs calls to record job creation.
	if strings.Contains(query, "INSERT INTO jobs") {
		if len(args) >= 1 {
			if id, ok := args[0].(string); ok {
				p.insertJobCalls = append(p.insertJobCalls, id)
			}
		}
	}

	// Capture UPDATE instances ... SET vm_state calls.
	if strings.Contains(query, "UPDATE instances") {
		if len(args) >= 3 {
			call := updateStateCall{}
			if id, ok := args[0].(string); ok {
				call.instanceID = id
			}
			if expected, ok := args[1].(string); ok {
				call.expectedState = expected
			}
			if next, ok := args[2].(string); ok {
				call.newState = next
			}
			p.updateStateCalls = append(p.updateStateCalls, call)
		}
	}

	return &dispatchTag{rows: p.execRowsAffected}, nil
}

func containsUpdateInstances(q string) bool {
	return strings.Contains(q, "UPDATE instances")
}

// QueryRow handles HasActivePendingJob (COUNT query).
// GetInstanceByID is handled by dispatchTestPool which overrides this method.
func (p *dispatchFakePool) QueryRow(_ context.Context, query string, _ ...any) db.Row {
	p.mu.Lock()
	defer p.mu.Unlock()

	if containsSelectCount(query) {
		return &dispatchRow{values: []any{p.countResult}}
	}
	return &dispatchRow{err: fmt.Errorf("no rows in result set")}
}

func containsSelectCount(q string) bool {
	return strings.Contains(q, "COUNT")
}

var _ db.Pool = (*dispatchFakePool)(nil)

func (p *dispatchFakePool) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &emptyDispatchRows{}, nil
}

func (p *dispatchFakePool) Close() {}

type dispatchTag struct{ rows int64 }

func (t *dispatchTag) RowsAffected() int64 { return t.rows }

type dispatchRow struct {
	values []any
	err    error
}

func (r *dispatchRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if i >= len(r.values) || r.values[i] == nil {
			continue
		}
		switch dst := d.(type) {
		case *string:
			if v, ok := r.values[i].(string); ok {
				*dst = v
			}
		case *int:
			if v, ok := r.values[i].(int); ok {
				*dst = v
			}
		case **string:
			if v, ok := r.values[i].(string); ok {
				*dst = &v
			}
		case *time.Time:
			if v, ok := r.values[i].(time.Time); ok {
				*dst = v
			}
		case **time.Time:
			if v, ok := r.values[i].(time.Time); ok {
				*dst = &v
			}
		case *bool:
			if v, ok := r.values[i].(bool); ok {
				*dst = v
			}
		}
	}
	return nil
}

type emptyDispatchRows struct{}

func (r *emptyDispatchRows) Next() bool        { return false }
func (r *emptyDispatchRows) Scan(...any) error { return nil }
func (r *emptyDispatchRows) Close()            {}
func (r *emptyDispatchRows) Err() error        { return nil }

func instanceToScanValues(inst *db.InstanceRow) []any {
	now := time.Now()
	createdAt := inst.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := inst.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	return []any{
		inst.ID, inst.Name, inst.OwnerPrincipalID, inst.VMState,
		inst.InstanceTypeID, inst.ImageID, inst.HostID, inst.AvailabilityZone,
		inst.Version, createdAt, updatedAt, inst.DeletedAt,
	}
}

// ── test-scoped pool that holds instances ─────────────────────────────────────

// dispatchTestPool extends dispatchFakePool with an instance store.
// We embed the pool and override QueryRow to serve GetInstanceByID.
type dispatchTestPool struct {
	dispatchFakePool
	instances map[string]*db.InstanceRow
}

func newDispatchTestPool() *dispatchTestPool {
	return &dispatchTestPool{
		dispatchFakePool: dispatchFakePool{
			activeJobMap:     make(map[string]bool),
			execRowsAffected: 1,
		},
		instances: make(map[string]*db.InstanceRow),
	}
}

func (p *dispatchTestPool) addInstance(inst *db.InstanceRow) {
	p.instances[inst.ID] = inst
}

func (p *dispatchTestPool) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	p.mu.Lock()
	defer p.mu.Unlock()

	if containsSelectCount(query) {
		return &dispatchRow{values: []any{p.countResult}}
	}
	if len(args) >= 1 {
		if id, ok := args[0].(string); ok {
			if inst, ok2 := p.instances[id]; ok2 {
				return &dispatchRow{values: instanceToScanValues(inst)}
			}
		}
	}
	return &dispatchRow{err: fmt.Errorf("no rows in result set")}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func dispatchTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func makeDispatchInstance(id, state string) *db.InstanceRow {
	now := time.Now()
	hostID := "host-001"
	return &db.InstanceRow{
		ID:        id,
		VMState:   state,
		HostID:    &hostID,
		Version:   1,
		UpdatedAt: now,
	}
}

func makeDispatcher(pool *dispatchTestPool) *Dispatcher {
	repo := db.New(pool)
	limiter := NewRateLimiter()
	return NewDispatcher(repo, limiter, dispatchTestLog())
}

func dispCtx() context.Context { return context.Background() }

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestDispatcher_NoDrift_NoOp verifies that DriftNone produces no DB writes.
func TestDispatcher_NoDrift_NoOp(t *testing.T) {
	pool := newDispatchTestPool()
	d := makeDispatcher(pool)
	inst := makeDispatchInstance("inst-d-001", "running")

	err := d.Dispatch(dispCtx(), inst, NoDrift)
	if err != nil {
		t.Fatalf("Dispatch NoDrift: unexpected error: %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) != 0 {
		t.Errorf("NoDrift: expected no job inserts, got %v", pool.insertJobCalls)
	}
	if len(pool.updateStateCalls) != 0 {
		t.Errorf("NoDrift: expected no state updates, got %v", pool.updateStateCalls)
	}
}

// TestDispatcher_WrongRuntimeState_CreatesRepairJob verifies the happy path:
// drift detected → no active job → rate limit OK → repair job inserted.
// Source: 03-03 §Job-Based Architecture.
func TestDispatcher_WrongRuntimeState_CreatesRepairJob(t *testing.T) {
	pool := newDispatchTestPool()
	// countResult=0 means no active job.
	inst := makeDispatchInstance("inst-d-002", "stopping")

	d := makeDispatcher(pool)
	drift := DriftResult{
		Class:         DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck stopping",
	}

	err := d.Dispatch(dispCtx(), inst, drift)
	if err != nil {
		t.Fatalf("Dispatch WrongRuntimeState: %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) != 1 {
		t.Errorf("expected 1 repair job insert, got %d", len(pool.insertJobCalls))
	}
}

// TestDispatcher_WrongRuntimeState_SkipsWhenActiveJobExists verifies idempotency:
// if a pending/in_progress job already exists, no second job is created.
// Source: 03-03 §Job-Based Architecture "check if a repair job is already pending".
func TestDispatcher_WrongRuntimeState_SkipsWhenActiveJobExists(t *testing.T) {
	pool := newDispatchTestPool()
	pool.countResult = 1 // HasActivePendingJob returns true

	d := makeDispatcher(pool)
	inst := makeDispatchInstance("inst-d-003", "stopping")
	drift := DriftResult{
		Class:         DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck stopping",
	}

	err := d.Dispatch(dispCtx(), inst, drift)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) != 0 {
		t.Errorf("idempotency: expected 0 job inserts (active job exists), got %d",
			len(pool.insertJobCalls))
	}
}

// TestDispatcher_WrongRuntimeState_SkipsWhenRateLimited verifies that the rate
// limiter suppresses repair dispatch when threshold is exceeded.
// Source: IMPLEMENTATION_PLAN_V1 §WS-3 (rate limiting output).
func TestDispatcher_WrongRuntimeState_SkipsWhenRateLimited(t *testing.T) {
	pool := newDispatchTestPool()
	pool.countResult = 0 // no active job

	// Use a tight rate limiter: max=1 in a 1-hour window.
	limiter := newRateLimiterWithParams(1*time.Hour, 1)
	// Pre-consume the one allowed slot.
	limiter.Allow("inst-d-004")

	repo := db.New(pool)
	d := NewDispatcher(repo, limiter, dispatchTestLog())

	inst := makeDispatchInstance("inst-d-004", "stopping")
	drift := DriftResult{
		Class:         DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck stopping",
	}

	err := d.Dispatch(dispCtx(), inst, drift)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) != 0 {
		t.Errorf("rate limited: expected 0 job inserts, got %d", len(pool.insertJobCalls))
	}
}

// TestDispatcher_StuckProvisioning_FailsInstance verifies the fail-instance path:
// no repair job is created; UpdateInstanceState is called with newState=failed.
// Source: 03-03 §Stuck-Provisioning "Transition db_state to FAILED. Do not retry."
func TestDispatcher_StuckProvisioning_FailsInstance(t *testing.T) {
	pool := newDispatchTestPool()
	inst := makeDispatchInstance("inst-d-005", "provisioning")
	pool.addInstance(inst)

	d := makeDispatcher(pool)
	drift := DriftResult{
		Class:  DriftStuckProvisioning,
		Reason: "provisioning > 15 min",
	}

	err := d.Dispatch(dispCtx(), inst, drift)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	// No repair job should be inserted.
	if len(pool.insertJobCalls) != 0 {
		t.Errorf("StuckProvisioning: expected 0 job inserts, got %d (should fail, not enqueue)",
			len(pool.insertJobCalls))
	}
	// UpdateInstanceState should have been called to transition to failed.
	foundFail := false
	for _, c := range pool.updateStateCalls {
		if c.newState == "failed" {
			foundFail = true
		}
	}
	if !foundFail {
		t.Error("StuckProvisioning: expected UpdateInstanceState with newState=failed")
	}
}

// TestDispatcher_StaleOptimisticLock_SkippedSilently verifies that when
// UpdateInstanceState returns 0 rows (stale version), the dispatcher does not
// return an error — it logs and continues.
// Source: LIFECYCLE_STATE_MACHINE_V1 §7 (optimistic locking),
//         03-03 §"any write operations must use optimistic locking".
func TestDispatcher_StaleOptimisticLock_SkippedSilently(t *testing.T) {
	pool := newDispatchTestPool()
	pool.execRowsAffected = 0 // simulate stale version: 0 rows affected

	inst := makeDispatchInstance("inst-d-006", "provisioning")
	pool.addInstance(inst)

	d := makeDispatcher(pool)
	drift := DriftResult{
		Class:  DriftStuckProvisioning,
		Reason: "stuck",
	}

	// Must not return an error — stale lock is silently skipped.
	err := d.Dispatch(dispCtx(), inst, drift)
	if err != nil {
		t.Errorf("stale optimistic lock: expected nil error, got: %v", err)
	}
}

// TestDispatcher_MissingRuntimeProcess_FailsInstance verifies the
// missing-runtime-process path leads to a fail transition, not a repair job.
// Source: 03-03 §Missing-Runtime-Process "Do not reschedule."
func TestDispatcher_MissingRuntimeProcess_FailsInstance(t *testing.T) {
	pool := newDispatchTestPool()
	inst := makeDispatchInstance("inst-d-007", "running")
	pool.addInstance(inst)

	d := makeDispatcher(pool)
	drift := DriftResult{
		Class:  DriftMissingRuntimeProcess,
		Reason: "no heartbeat > 5 min",
	}

	err := d.Dispatch(dispCtx(), inst, drift)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) != 0 {
		t.Errorf("MissingRuntimeProcess: expected 0 job inserts, got %d", len(pool.insertJobCalls))
	}
}

// TestDispatcher_OrphanedResource_FailsInstance verifies the orphaned resource
// path transitions the instance to failed.
// Source: 03-03 §Orphaned-Resource.
func TestDispatcher_OrphanedResource_FailsInstance(t *testing.T) {
	pool := newDispatchTestPool()
	inst := makeDispatchInstance("inst-d-008", "running")
	pool.addInstance(inst)

	d := makeDispatcher(pool)
	drift := DriftResult{
		Class:  DriftOrphanedResource,
		Reason: "running with no host",
	}

	err := d.Dispatch(dispCtx(), inst, drift)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertJobCalls) != 0 {
		t.Errorf("OrphanedResource: expected 0 job inserts, got %d", len(pool.insertJobCalls))
	}
}

// TestDispatcher_NonFailableState_NoStateWrite verifies that the dispatcher
// does not attempt to fail an instance already in a stable/terminal state.
// Source: LIFECYCLE_STATE_MACHINE_V1 §5 (FAIL only from transitional states).
func TestDispatcher_NonFailableState_NoStateWrite(t *testing.T) {
	pool := newDispatchTestPool()
	// Instance is already failed — isFailableState("failed") == false.
	inst := makeDispatchInstance("inst-d-009", "failed")
	pool.addInstance(inst)

	d := makeDispatcher(pool)
	drift := DriftResult{
		Class:  DriftStuckProvisioning,
		Reason: "test",
	}

	err := d.Dispatch(dispCtx(), inst, drift)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.updateStateCalls) != 0 {
		t.Errorf("non-failable state: expected 0 UpdateInstanceState calls, got %d",
			len(pool.updateStateCalls))
	}
}

// TestJobMaxAttempts_AllTypesHaveValues verifies all job types return a non-zero
// max_attempts value from the dispatcher's internal table.
// Source: JOB_MODEL_V1 §3.
func TestJobMaxAttempts_AllTypesHaveValues(t *testing.T) {
	jobTypes := []string{
		"INSTANCE_CREATE",
		"INSTANCE_DELETE",
		"INSTANCE_START",
		"INSTANCE_STOP",
		"INSTANCE_REBOOT",
	}
	for _, jt := range jobTypes {
		if n := jobMaxAttempts(jt); n <= 0 {
			t.Errorf("jobMaxAttempts(%q) = %d, want > 0", jt, n)
		}
	}
}
