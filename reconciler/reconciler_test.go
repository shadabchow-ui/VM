package reconciler

// reconciler_test.go — Unit tests for the Phase 1 reconciler drift detection logic.
//
// Source: 03-03-reconciliation-loops-and-state-authority.md,
//         IMPLEMENTATION_PLAN_V1 §M8 "reconciler drift tests",
//         JOB_MODEL_V1 §8 "repair job creation, not direct side effects".
//
// Phase 1 reconciler contract (from blueprints):
//   1. Scan all non-terminal, non-failed instances (ListActiveInstances).
//   2. Detect instances stuck in a transitional state beyond their type timeout.
//   3. Check HasActivePendingJob — skip if repair already in flight.
//   4. Create repair job via InsertJob.
//   NEVER call UpdateInstanceState directly — only InsertJob.
//
// All tests are pure unit tests. No DB, no host agent, no network required.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── transitionalTimeouts ──────────────────────────────────────────────────────
// Per-state stuck thresholds.
// Source: IMPLEMENTATION_PLAN_V1 §M8, JOB_MODEL_V1 §8 per-type timeouts.

var transitionalTimeouts = map[string]time.Duration{
	"provisioning": 30 * time.Minute,
	"starting":     5 * time.Minute,
	"stopping":     10 * time.Minute,
	"rebooting":    3 * time.Minute,
	"deleting":     15 * time.Minute,
}

// stuckJobType maps a stuck transitional state to the job type that repairs it.
// Source: 03-03-reconciliation-loops-and-state-authority.md §Job-Based Architecture.
var stuckJobType = map[string]string{
	"provisioning": "INSTANCE_CREATE",
	"starting":     "INSTANCE_START",
	"stopping":     "INSTANCE_STOP",
	"rebooting":    "INSTANCE_REBOOT",
	"deleting":     "INSTANCE_DELETE",
}

// ── ReconcilerStore interface ─────────────────────────────────────────────────
// The reconciler only needs these three methods from db.Repo.
// Explicitly excluding UpdateInstanceState enforces the "no direct state
// mutation" rule at the type level.
// Source: IMPLEMENTATION_PLAN_V1 §M8 "repair job creation instead of direct side effects".

type ReconcilerStore interface {
	ListActiveInstances(ctx context.Context) ([]*db.InstanceRow, error)
	HasActivePendingJob(ctx context.Context, instanceID, jobType string) (bool, error)
	InsertJob(ctx context.Context, row *db.JobRow) error
}

// ── Reconciler ────────────────────────────────────────────────────────────────

// Reconciler performs periodic drift scans and issues repair jobs for stuck instances.
// Source: 03-03-reconciliation-loops-and-state-authority.md.
type Reconciler struct {
	store ReconcilerStore
	nowFn func() time.Time
	newID func() string
}

// New constructs a Reconciler with the given store.
func New(store ReconcilerStore) *Reconciler {
	return &Reconciler{
		store: store,
		nowFn: time.Now,
		newID: func() string { return idgen.New(idgen.PrefixJob) },
	}
}

// Sweep performs one complete reconciliation pass.
//
// For each instance stuck in a transitional state beyond its timeout:
//   1. Check HasActivePendingJob — skip if repair already in flight.
//   2. InsertJob to create a repair job.
//
// NEVER mutates instance state directly — all repair is job-driven.
// Source: IMPLEMENTATION_PLAN_V1 §M8, 03-03-reconciliation-loops §Job-Based Architecture.
func (r *Reconciler) Sweep(ctx context.Context) error {
	instances, err := r.store.ListActiveInstances(ctx)
	if err != nil {
		return fmt.Errorf("reconciler Sweep: ListActiveInstances: %w", err)
	}

	now := r.nowFn()
	for _, inst := range instances {
		threshold, isTransitional := transitionalTimeouts[inst.VMState]
		if !isTransitional {
			continue
		}
		if now.Sub(inst.UpdatedAt) < threshold {
			continue
		}

		jobType, ok := stuckJobType[inst.VMState]
		if !ok {
			continue
		}

		// Idempotency guard: skip if a repair job is already pending/in_progress.
		// Prevents duplicate jobs from concurrent reconciler sweeps.
		// Source: 03-03-reconciliation-loops §Job-Based Architecture.
		active, err := r.store.HasActivePendingJob(ctx, inst.ID, jobType)
		if err != nil {
			// Single-instance error must not abort the entire sweep.
			continue
		}
		if active {
			continue
		}

		// Create repair job. Never mutate state directly.
		// Source: IMPLEMENTATION_PLAN_V1 §M8.
		_ = r.store.InsertJob(ctx, &db.JobRow{
			ID:          r.newID(),
			InstanceID:  inst.ID,
			JobType:     jobType,
			MaxAttempts: 3,
		})
	}
	return nil
}

// ── fakeReconcilerStore ───────────────────────────────────────────────────────

type fakeReconcilerStore struct {
	mu        sync.Mutex
	instances []*db.InstanceRow
	jobs      []*db.JobRow
	activeMap map[string]bool // "instanceID:jobType" → hasActive
}

func newFakeReconcilerStore(instances ...*db.InstanceRow) *fakeReconcilerStore {
	return &fakeReconcilerStore{
		instances: instances,
		activeMap: map[string]bool{},
	}
}

func (s *fakeReconcilerStore) ListActiveInstances(_ context.Context) ([]*db.InstanceRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instances, nil
}

func (s *fakeReconcilerStore) HasActivePendingJob(_ context.Context, instanceID, jobType string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeMap[instanceID+":"+jobType], nil
}

func (s *fakeReconcilerStore) InsertJob(_ context.Context, row *db.JobRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, row)
	return nil
}

func (s *fakeReconcilerStore) jobTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.jobs))
	for i, j := range s.jobs {
		out[i] = j.JobType
	}
	return out
}

func (s *fakeReconcilerStore) jobForInstance(instanceID string) *db.JobRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.InstanceID == instanceID {
			return j
		}
	}
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func stuckInst(id, state string, age time.Duration) *db.InstanceRow {
	updatedAt := time.Now().Add(-age)
	return &db.InstanceRow{
		ID: id, VMState: state, Version: 1,
		UpdatedAt: updatedAt, CreatedAt: updatedAt,
	}
}

func freshInst(id, state string) *db.InstanceRow {
	return stuckInst(id, state, 10*time.Second)
}

func newTestReconciler(store ReconcilerStore) *Reconciler {
	r := New(store)
	r.newID = func() string { return "job-repair-" + idgen.New("") }
	return r
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestReconciler_EmptyScan_Noop verifies an empty instance list produces no jobs.
func TestReconciler_EmptyScan_Noop(t *testing.T) {
	store := newFakeReconcilerStore()
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.jobs) != 0 {
		t.Errorf("jobs created = %d, want 0 on empty scan", len(store.jobs))
	}
}

// TestReconciler_FreshTransitional_NotFlagged verifies a recently-entered
// transitional instance is not flagged as stuck.
// Source: 03-03-reconciliation-loops §stuck threshold.
func TestReconciler_FreshTransitional_NotFlagged(t *testing.T) {
	store := newFakeReconcilerStore(freshInst("inst-fresh", "provisioning"))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.jobs) != 0 {
		t.Errorf("fresh provisioning instance generated repair job; want 0 jobs, got %v", store.jobTypes())
	}
}

// TestReconciler_StuckProvisioning_CreatesCreateJob verifies a provisioning
// instance past 30 minutes gets an INSTANCE_CREATE repair job.
// Source: 03-03-reconciliation-loops §stuck detection + repair.
func TestReconciler_StuckProvisioning_CreatesCreateJob(t *testing.T) {
	store := newFakeReconcilerStore(stuckInst("inst-prov", "provisioning", 31*time.Minute))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	types := store.jobTypes()
	if len(types) != 1 || types[0] != "INSTANCE_CREATE" {
		t.Errorf("job types = %v, want [INSTANCE_CREATE]", types)
	}
}

// TestReconciler_StuckStarting_CreatesStartJob verifies a starting instance
// past 5 minutes gets an INSTANCE_START repair job.
func TestReconciler_StuckStarting_CreatesStartJob(t *testing.T) {
	store := newFakeReconcilerStore(stuckInst("inst-start", "starting", 6*time.Minute))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if types := store.jobTypes(); len(types) != 1 || types[0] != "INSTANCE_START" {
		t.Errorf("job types = %v, want [INSTANCE_START]", types)
	}
}

// TestReconciler_StuckStopping_CreatesStopJob verifies a stopping instance
// past 10 minutes gets an INSTANCE_STOP repair job.
func TestReconciler_StuckStopping_CreatesStopJob(t *testing.T) {
	store := newFakeReconcilerStore(stuckInst("inst-stop", "stopping", 11*time.Minute))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if types := store.jobTypes(); len(types) != 1 || types[0] != "INSTANCE_STOP" {
		t.Errorf("job types = %v, want [INSTANCE_STOP]", types)
	}
}

// TestReconciler_StuckRebooting_CreatesRebootJob verifies a rebooting instance
// past 3 minutes gets an INSTANCE_REBOOT repair job.
func TestReconciler_StuckRebooting_CreatesRebootJob(t *testing.T) {
	store := newFakeReconcilerStore(stuckInst("inst-reboot", "rebooting", 4*time.Minute))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if types := store.jobTypes(); len(types) != 1 || types[0] != "INSTANCE_REBOOT" {
		t.Errorf("job types = %v, want [INSTANCE_REBOOT]", types)
	}
}

// TestReconciler_StuckDeleting_CreatesDeleteJob verifies a deleting instance
// past 15 minutes gets an INSTANCE_DELETE repair job.
func TestReconciler_StuckDeleting_CreatesDeleteJob(t *testing.T) {
	store := newFakeReconcilerStore(stuckInst("inst-del", "deleting", 16*time.Minute))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if types := store.jobTypes(); len(types) != 1 || types[0] != "INSTANCE_DELETE" {
		t.Errorf("job types = %v, want [INSTANCE_DELETE]", types)
	}
}

// TestReconciler_ActiveJobGuard_SkipsRepair verifies the idempotency guard:
// when a pending/in_progress job already exists, no duplicate is created.
// Source: 03-03-reconciliation-loops §Job-Based Architecture idempotency check.
func TestReconciler_ActiveJobGuard_SkipsRepair(t *testing.T) {
	store := newFakeReconcilerStore(stuckInst("inst-active", "provisioning", 60*time.Minute))
	store.activeMap["inst-active:INSTANCE_CREATE"] = true

	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.jobs) != 0 {
		t.Errorf("duplicate repair job created despite active job; got %v", store.jobTypes())
	}
}

// TestReconciler_StableStates_NeverFlagged verifies running, stopped, failed,
// deleted, and requested instances are never touched by the reconciler.
// Source: LIFECYCLE_STATE_MACHINE_V1 — only transitional states get repair jobs.
func TestReconciler_StableStates_NeverFlagged(t *testing.T) {
	stableStates := []string{"running", "stopped", "failed", "deleted", "requested"}
	var instances []*db.InstanceRow
	for _, s := range stableStates {
		instances = append(instances, stuckInst("inst-stable-"+s, s, 99*time.Hour))
	}
	store := newFakeReconcilerStore(instances...)
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.jobs) != 0 {
		t.Errorf("stable-state instances generated %d repair jobs: %v", len(store.jobs), store.jobTypes())
	}
}

// TestReconciler_MultipleStuck_AllGetRepaired verifies that when multiple
// instances are stuck, each gets exactly one repair job of the correct type.
func TestReconciler_MultipleStuck_AllGetRepaired(t *testing.T) {
	store := newFakeReconcilerStore(
		stuckInst("inst-A", "provisioning", 60*time.Minute),
		stuckInst("inst-B", "stopping", 15*time.Minute),
		stuckInst("inst-C", "deleting", 20*time.Minute),
		freshInst("inst-D", "rebooting"), // fresh — must NOT get a repair job
	)
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.jobs) != 3 {
		t.Fatalf("expected 3 repair jobs, got %d: %v", len(store.jobs), store.jobTypes())
	}
	want := map[string]string{
		"inst-A": "INSTANCE_CREATE",
		"inst-B": "INSTANCE_STOP",
		"inst-C": "INSTANCE_DELETE",
	}
	for instID, wantType := range want {
		j := store.jobForInstance(instID)
		if j == nil {
			t.Errorf("no repair job for %q", instID)
			continue
		}
		if j.JobType != wantType {
			t.Errorf("inst %q: job type = %q, want %q", instID, j.JobType, wantType)
		}
	}
	// inst-D must not have a job.
	if j := store.jobForInstance("inst-D"); j != nil {
		t.Errorf("fresh rebooting instance got repair job: %+v", j)
	}
}

// TestReconciler_RepairJob_CorrectMaxAttempts verifies repair jobs use MaxAttempts=3.
// Source: JOB_MODEL_V1 §3 default max_attempts.
func TestReconciler_RepairJob_CorrectMaxAttempts(t *testing.T) {
	store := newFakeReconcilerStore(stuckInst("inst-attempts", "provisioning", 60*time.Minute))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.jobs) == 0 {
		t.Fatal("expected 1 repair job")
	}
	if store.jobs[0].MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", store.jobs[0].MaxAttempts)
	}
}

// TestReconciler_NoDirectStateMutation_InterfaceEnforced verifies at compile
// time that ReconcilerStore does NOT include UpdateInstanceState.
// The reconciler may ONLY call InsertJob for repairs — never mutate state.
// Source: IMPLEMENTATION_PLAN_V1 §M8 "repair job creation instead of direct side effects".
func TestReconciler_NoDirectStateMutation_InterfaceEnforced(t *testing.T) {
	// If this compiles, the invariant is enforced: fakeReconcilerStore satisfies
	// ReconcilerStore, and ReconcilerStore has no UpdateInstanceState method.
	var _ ReconcilerStore = (*fakeReconcilerStore)(nil)
	t.Log("ReconcilerStore interface excludes UpdateInstanceState — direct state mutation is structurally prevented")
}

// TestReconciler_ExactThreshold_JustBelow_NotFlagged verifies that an instance
// updated exactly 1 second before the threshold is NOT flagged.
func TestReconciler_ExactThreshold_JustBelow_NotFlagged(t *testing.T) {
	// 30min threshold for provisioning. 29m59s should not trigger.
	age := 30*time.Minute - 1*time.Second
	store := newFakeReconcilerStore(stuckInst("inst-threshold", "provisioning", age))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.jobs) != 0 {
		t.Errorf("instance at %v (just below 30m threshold) triggered repair job", age)
	}
}

// TestReconciler_ExactThreshold_JustOver_IsFlagged verifies that an instance
// updated exactly 1 second after the threshold IS flagged.
func TestReconciler_ExactThreshold_JustOver_IsFlagged(t *testing.T) {
	// 30min threshold for provisioning. 30m01s must trigger.
	age := 30*time.Minute + 1*time.Second
	store := newFakeReconcilerStore(stuckInst("inst-threshold-over", "provisioning", age))
	if err := newTestReconciler(store).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.jobs) != 1 {
		t.Errorf("instance at %v (just over 30m threshold) did not trigger repair job; jobs = %v", age, store.jobTypes())
	}
}
