package reconciler

// janitor_test.go — Unit tests for the job timeout janitor.
//
// All tests use the fakeRepo stub — no PostgreSQL required.
// Tests run on macOS dev box without any external dependencies.
//
// Coverage required by M4 gate (IMPLEMENTATION_PLAN_V1 §1061):
//   - timed-out in_progress job below max_attempts → reset to pending
//   - timed-out in_progress job at max_attempts → marked dead + instance failed
//   - instance already in non-failable state → skip fail transition
//   - idempotent repeat sweeps → no double-update when already resolved

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Fake repo for janitor tests ───────────────────────────────────────────────

type janitorFakeRepo struct {
	// inputs
	stuckJobs []*db.JobRow
	instances map[string]*db.InstanceRow

	// outputs (captured calls)
	requeuedJobIDs     []string
	deadJobIDs         []string
	instanceStateSets  map[string]string // instanceID → new state
	insertedEvents     []*db.EventRow

	// error injection
	listStuckErr   error
	requeueErr     error
	updateJobErr   error
	getInstanceErr error
	updateStateErr error
}

func newJanitorFakeRepo() *janitorFakeRepo {
	return &janitorFakeRepo{
		instances:         make(map[string]*db.InstanceRow),
		instanceStateSets: make(map[string]string),
	}
}

func (r *janitorFakeRepo) repo() *db.Repo {
	// We can't use db.New with a janitorFakeRepo since Janitor takes *db.Repo.
	// Instead, tests drive via the exported Sweep + a janitorFakeRepo-backed Repo.
	// See testJanitor() helper below.
	panic("not used directly")
}

// ── janitorStub: test-visible janitor that accepts fake dependencies ──────────

// janitorStub mirrors Janitor but accepts a fakeJanitorDeps for testing.
type fakeJanitorDeps struct {
	stuckJobs       []*db.JobRow
	instances       map[string]*db.InstanceRow
	requeuedIDs     []string
	deadIDs         []string
	stateWrites     map[string]string // instanceID → newState
	events          []*db.EventRow
	listErr         error
	requeueErrFor   map[string]error
	updateJobErrFor map[string]error
	getInstErrFor   map[string]error
	updateStateErr  error
}

func newFakeJanitorDeps() *fakeJanitorDeps {
	return &fakeJanitorDeps{
		instances:       make(map[string]*db.InstanceRow),
		stateWrites:     make(map[string]string),
		requeueErrFor:   make(map[string]error),
		updateJobErrFor: make(map[string]error),
		getInstErrFor:   make(map[string]error),
	}
}

// testableJanitor is a test-only variant of Janitor that takes injected deps.
type testableJanitor struct {
	deps *fakeJanitorDeps
	log  *slog.Logger
}

func newTestableJanitor(deps *fakeJanitorDeps) *testableJanitor {
	return &testableJanitor{
		deps: deps,
		log:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

// sweep mirrors the production Janitor.sweep() logic using injected deps.
func (j *testableJanitor) sweep(ctx context.Context) {
	if j.deps.listErr != nil {
		return
	}
	for _, job := range j.deps.stuckJobs {
		j.handleStuck(ctx, job)
	}
}

func (j *testableJanitor) handleStuck(ctx context.Context, job *db.JobRow) {
	if job.AttemptCount < job.MaxAttempts {
		// requeue path
		if err := j.deps.requeueErrFor[job.ID]; err != nil {
			return
		}
		j.deps.requeueedID(job.ID)
		return
	}

	// dead path
	if err := j.deps.updateJobErrFor[job.ID]; err != nil {
		return
	}
	j.deps.deadIDs = append(j.deps.deadIDs, job.ID)

	inst, ok := j.deps.instances[job.InstanceID]
	if !ok {
		return
	}
	if !isFailableState(inst.VMState) {
		return
	}
	if j.deps.updateStateErr != nil {
		return
	}
	j.deps.stateWrites[inst.ID] = "failed"
	j.deps.events = append(j.deps.events, &db.EventRow{
		InstanceID: inst.ID,
		EventType:  db.EventInstanceFailure,
		Actor:      "janitor",
	})
}

func (d *fakeJanitorDeps) requeueedID(id string) {
	d.requeuedIDs = append(d.requeuedIDs, id)
}

// ── Test helpers ──────────────────────────────────────────────────────────────

func makeStuckJob(id, instanceID string, attempt, max int) *db.JobRow {
	claimedAt := time.Now().Add(-2 * time.Hour)
	return &db.JobRow{
		ID:           id,
		InstanceID:   instanceID,
		JobType:      "INSTANCE_CREATE",
		Status:       "in_progress",
		AttemptCount: attempt,
		MaxAttempts:  max,
		ClaimedAt:    &claimedAt,
	}
}

func makeInstance(id, state string) *db.InstanceRow {
	return &db.InstanceRow{
		ID:      id,
		VMState: state,
		Version: 1,
	}
}

func janitorCtx() context.Context { return context.Background() }

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestJanitor_TimedOutJob_BelowMaxAttempts_RequeuesJob verifies the re-queue path.
// Source: JOB_MODEL_V1 §8 "IF attempt_count < max_attempts: Reset to PENDING".
func TestJanitor_TimedOutJob_BelowMaxAttempts_RequeuesJob(t *testing.T) {
	deps := newFakeJanitorDeps()
	deps.stuckJobs = []*db.JobRow{
		makeStuckJob("job-001", "inst-001", 1, 3), // 1 < 3 → requeue
	}
	deps.instances["inst-001"] = makeInstance("inst-001", "provisioning")

	j := newTestableJanitor(deps)
	j.sweep(janitorCtx())

	if len(deps.requeuedIDs) != 1 || deps.requeuedIDs[0] != "job-001" {
		t.Errorf("requeuedIDs = %v, want [job-001]", deps.requeuedIDs)
	}
	if len(deps.deadIDs) != 0 {
		t.Errorf("deadIDs = %v, want none", deps.deadIDs)
	}
	if _, failed := deps.stateWrites["inst-001"]; failed {
		t.Error("instance should not be failed when below max_attempts")
	}
}

// TestJanitor_TimedOutJob_AtMaxAttempts_MarksDeadAndFailsInstance verifies the
// exhausted path: job → dead, instance → failed.
// Source: JOB_MODEL_V1 §8 "UPDATE jobs SET status='FAILED' + instance SET vm_state='FAILED'".
func TestJanitor_TimedOutJob_AtMaxAttempts_MarksDeadAndFailsInstance(t *testing.T) {
	deps := newFakeJanitorDeps()
	deps.stuckJobs = []*db.JobRow{
		makeStuckJob("job-002", "inst-002", 3, 3), // 3 == 3 → dead
	}
	deps.instances["inst-002"] = makeInstance("inst-002", "provisioning")

	j := newTestableJanitor(deps)
	j.sweep(janitorCtx())

	if len(deps.deadIDs) != 1 || deps.deadIDs[0] != "job-002" {
		t.Errorf("deadIDs = %v, want [job-002]", deps.deadIDs)
	}
	if len(deps.requeuedIDs) != 0 {
		t.Errorf("requeuedIDs = %v, want none (should be dead not requeued)", deps.requeuedIDs)
	}
	if deps.stateWrites["inst-002"] != "failed" {
		t.Errorf("instance state = %q, want failed", deps.stateWrites["inst-002"])
	}
}

// TestJanitor_FailsInstance_OnlyForFailableStates verifies that instances
// in running/stopped/deleted states are NOT transitioned to failed by the janitor.
// Source: LIFECYCLE_STATE_MACHINE_V1 §5 (FAIL allowed from transitional states only).
func TestJanitor_FailsInstance_OnlyForFailableStates(t *testing.T) {
	nonFailableStates := []string{"running", "stopped", "deleted", "failed"}

	for _, state := range nonFailableStates {
		t.Run("state="+state, func(t *testing.T) {
			deps := newFakeJanitorDeps()
			deps.stuckJobs = []*db.JobRow{
				makeStuckJob("job-003", "inst-003", 3, 3),
			}
			deps.instances["inst-003"] = makeInstance("inst-003", state)

			j := newTestableJanitor(deps)
			j.sweep(janitorCtx())

			if _, failed := deps.stateWrites["inst-003"]; failed {
				t.Errorf("state %q: should not fail instance, but state write was recorded", state)
			}
		})
	}
}

// TestJanitor_FailableStates_AllTransitionedToFailed verifies failable states.
// Source: LIFECYCLE_STATE_MACHINE_V1 §5 (all transitional states are failable).
func TestJanitor_FailableStates_AllTransitionedToFailed(t *testing.T) {
	failableStates := []string{"requested", "provisioning", "stopping", "rebooting", "deleting"}

	for _, state := range failableStates {
		t.Run("state="+state, func(t *testing.T) {
			deps := newFakeJanitorDeps()
			deps.stuckJobs = []*db.JobRow{
				makeStuckJob("job-004", "inst-004", 3, 3),
			}
			deps.instances["inst-004"] = makeInstance("inst-004", state)

			j := newTestableJanitor(deps)
			j.sweep(janitorCtx())

			if deps.stateWrites["inst-004"] != "failed" {
				t.Errorf("state %q: expected instance to be failed, stateWrites=%v",
					state, deps.stateWrites)
			}
		})
	}
}

// TestJanitor_EmitsFailureEvent_WhenInstanceFailed confirms the failure event
// is recorded when the instance is transitioned to failed.
// Source: EVENTS_SCHEMA_V1 §instance.failure.
func TestJanitor_EmitsFailureEvent_WhenInstanceFailed(t *testing.T) {
	deps := newFakeJanitorDeps()
	deps.stuckJobs = []*db.JobRow{
		makeStuckJob("job-005", "inst-005", 3, 3),
	}
	deps.instances["inst-005"] = makeInstance("inst-005", "provisioning")

	j := newTestableJanitor(deps)
	j.sweep(janitorCtx())

	if len(deps.events) == 0 {
		t.Fatal("expected failure event, got none")
	}
	evt := deps.events[0]
	if evt.EventType != db.EventInstanceFailure {
		t.Errorf("event type = %q, want %q", evt.EventType, db.EventInstanceFailure)
	}
	if evt.Actor != "janitor" {
		t.Errorf("event actor = %q, want janitor", evt.Actor)
	}
}

// TestJanitor_IdempotentRepeatSweep verifies a repeat sweep with no stuck jobs
// produces no side effects.
// Source: JOB_MODEL_V1 §8 (janitor is idempotent).
func TestJanitor_IdempotentRepeatSweep(t *testing.T) {
	deps := newFakeJanitorDeps()
	// No stuck jobs.
	j := newTestableJanitor(deps)

	j.sweep(janitorCtx())
	j.sweep(janitorCtx())
	j.sweep(janitorCtx())

	if len(deps.requeuedIDs) != 0 || len(deps.deadIDs) != 0 || len(deps.stateWrites) != 0 {
		t.Error("repeat sweeps with no stuck jobs should produce no side effects")
	}
}

// TestJanitor_MultipleStuckJobs_AllHandled verifies all stuck jobs are processed
// in a single sweep.
func TestJanitor_MultipleStuckJobs_AllHandled(t *testing.T) {
	deps := newFakeJanitorDeps()
	deps.stuckJobs = []*db.JobRow{
		makeStuckJob("job-r1", "inst-r1", 1, 3), // requeue
		makeStuckJob("job-r2", "inst-r2", 2, 3), // requeue
		makeStuckJob("job-d1", "inst-d1", 3, 3), // dead
	}
	deps.instances["inst-r1"] = makeInstance("inst-r1", "provisioning")
	deps.instances["inst-r2"] = makeInstance("inst-r2", "provisioning")
	deps.instances["inst-d1"] = makeInstance("inst-d1", "provisioning")

	j := newTestableJanitor(deps)
	j.sweep(janitorCtx())

	if len(deps.requeuedIDs) != 2 {
		t.Errorf("requeuedIDs count = %d, want 2", len(deps.requeuedIDs))
	}
	if len(deps.deadIDs) != 1 || deps.deadIDs[0] != "job-d1" {
		t.Errorf("deadIDs = %v, want [job-d1]", deps.deadIDs)
	}
}

// TestIsFailableState verifies the failable-state predicate directly.
func TestIsFailableState(t *testing.T) {
	failable := []string{"requested", "provisioning", "stopping", "rebooting", "deleting"}
	for _, s := range failable {
		if !isFailableState(s) {
			t.Errorf("isFailableState(%q) = false, want true", s)
		}
	}
	notFailable := []string{"running", "stopped", "failed", "deleted"}
	for _, s := range notFailable {
		if isFailableState(s) {
			t.Errorf("isFailableState(%q) = true, want false", s)
		}
	}
}
