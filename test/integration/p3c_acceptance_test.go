//go:build integration

package integration

// p3c_acceptance_test.go — VM-P3C rollout controls acceptance gate tests.
//
// These tests verify the VM-P3C rollout gate contracts using only
// exported package APIs. No unexported fields are accessed.
//
// Coverage:
//   RolloutGate lifecycle:
//     - Fresh gate is not paused
//     - Pause → IsPaused true, Status reflects reason and timestamp
//     - Resume → IsPaused false, Status cleared
//     - Pause/Resume cycle is idempotent (no panics)
//     - Concurrent access is race-free (run with -race)
//
//   Dispatcher gate integration:
//     - DriftWrongRuntimeState: repair job suppressed when gate is paused
//     - DriftStuckProvisioning: failInstance is NOT suppressed (state correction safe)
//     - After gate.Resume: repair job insertion proceeds normally
//
//   DB-level acceptance (requires DATABASE_URL):
//     - No job row written to DB when gate is paused + DriftWrongRuntimeState
//
// Environment:
//   DATABASE_URL=postgres://... — required only for TestP3C_DB_* tests.
//   All other tests in this file run without a DB.
//
// Run:
//   # All P3C acceptance tests (non-DB):
//   go test -tags=integration -v ./test/integration/... -run TestP3C
//
//   # DB-level gate test:
//   DATABASE_URL=postgres://... go test -tags=integration -v ./test/integration/... -run TestP3C_DB
//
//   # With race detector:
//   go test -race -tags=integration -v ./test/integration/... -run TestP3C
//
// Source: VM_PHASE_ROADMAP §9 "deeper automation and rollout controls",
//         P2_M1_GATE_CHECKLIST §Q-1 (job idempotency integration tests),
//         P2_M1_WS_H7_PHASE1_REGRESSION_RUNBOOK §"Reconciler stability".

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
	"github.com/compute-platform/compute-platform/services/reconciler"
)

// ── RolloutGate lifecycle tests ───────────────────────────────────────────────

func TestP3C_RolloutGate_FreshGate_NotPaused(t *testing.T) {
	gate := reconciler.NewRolloutGate()
	if gate.IsPaused() {
		t.Error("fresh gate must not be paused on startup")
	}
	s := gate.Status()
	if s.Paused {
		t.Error("fresh gate Status().Paused must be false")
	}
	if s.PausedAt != nil {
		t.Errorf("fresh gate Status().PausedAt must be nil, got %v", *s.PausedAt)
	}
	if s.Reason != "" {
		t.Errorf("fresh gate Status().Reason must be empty, got %q", s.Reason)
	}
}

func TestP3C_RolloutGate_PauseResume_Cycle(t *testing.T) {
	gate := reconciler.NewRolloutGate()

	before := time.Now()
	gate.Pause("deploying worker v1.5.0")

	if !gate.IsPaused() {
		t.Fatal("gate must be paused after Pause()")
	}
	s := gate.Status()
	if !s.Paused {
		t.Error("Status().Paused must be true")
	}
	if s.PausedAt == nil {
		t.Fatal("Status().PausedAt must not be nil")
	}
	if s.PausedAt.Before(before) {
		t.Errorf("Status().PausedAt (%v) must not be before Pause() call (%v)", *s.PausedAt, before)
	}
	if s.Reason != "deploying worker v1.5.0" {
		t.Errorf("Status().Reason = %q, want the operator-supplied reason", s.Reason)
	}

	gate.Resume()

	if gate.IsPaused() {
		t.Fatal("gate must not be paused after Resume()")
	}
	s = gate.Status()
	if s.Paused {
		t.Error("Status().Paused must be false after Resume()")
	}
	if s.PausedAt != nil {
		t.Errorf("Status().PausedAt must be nil after Resume(), got %v", *s.PausedAt)
	}
	if s.Reason != "" {
		t.Errorf("Status().Reason must be empty after Resume(), got %q", s.Reason)
	}
}

func TestP3C_RolloutGate_Pause_IsIdempotent(t *testing.T) {
	gate := reconciler.NewRolloutGate()
	gate.Pause("first")
	gate.Pause("second") // must not panic
	if !gate.IsPaused() {
		t.Error("gate must remain paused after second Pause()")
	}
}

func TestP3C_RolloutGate_Resume_IsIdempotent(t *testing.T) {
	gate := reconciler.NewRolloutGate()
	gate.Resume()
	gate.Resume() // must not panic
	if gate.IsPaused() {
		t.Error("gate must remain resumed after second Resume()")
	}
}

func TestP3C_RolloutGate_ConcurrentAccess_NoRace(t *testing.T) {
	gate := reconciler.NewRolloutGate()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); gate.Pause("concurrent") }()
		go func() { defer wg.Done(); gate.Resume() }()
		go func() { defer wg.Done(); _ = gate.IsPaused(); _ = gate.Status() }()
	}
	wg.Wait()
	// No assertion — race detector catches problems when run with -race.
}

// ── Dispatcher gate integration tests (fake pool — no DB) ─────────────────────

// p3cFakePool is a minimal db.Pool for dispatcher gate tests.
type p3cFakePool struct {
	mu               sync.Mutex
	execCalls        int // total Exec calls
	insertJobCalls   int // INSERT INTO jobs calls
	updateStateCalls int // UPDATE instances calls
	countResult      int // HasActivePendingJob result
}

func (p *p3cFakePool) Exec(_ context.Context, query string, _ ...any) (db.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.execCalls++
	// Detect job inserts by SQL keyword presence.
	if contains(query, "INSERT") && contains(query, "job") {
		p.insertJobCalls++
	}
	if contains(query, "UPDATE") && contains(query, "instance") {
		p.updateStateCalls++
	}
	return &p3cFakeTag{rows: 1}, nil
}

func (p *p3cFakePool) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &p3cFakeRow{values: []any{p.countResult}}
}

func (p *p3cFakePool) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &p3cFakeRows{}, nil
}
func (p *p3cFakePool) Close() {}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

type p3cFakeTag struct{ rows int64 }

func (t *p3cFakeTag) RowsAffected() int64 { return t.rows }

type p3cFakeRow struct {
	values []any
}

func (r *p3cFakeRow) Scan(dest ...any) error {
	for i, d := range dest {
		if i >= len(r.values) || r.values[i] == nil {
			continue
		}
		switch dst := d.(type) {
		case *int:
			if v, ok := r.values[i].(int); ok {
				*dst = v
			}
		case *string:
			if v, ok := r.values[i].(string); ok {
				*dst = v
			}
		}
	}
	return nil
}

type p3cFakeRows struct{}

func (r *p3cFakeRows) Next() bool        { return false }
func (r *p3cFakeRows) Scan(...any) error { return nil }
func (r *p3cFakeRows) Close()            {}
func (r *p3cFakeRows) Err() error        { return nil }

func p3cTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func makeP3CDispatcher(pool *p3cFakePool, gate *reconciler.RolloutGate) *reconciler.Dispatcher {
	repo := db.New(pool)
	limiter := reconciler.NewRateLimiter()
	d := reconciler.NewDispatcher(repo, limiter, p3cTestLog())
	if gate != nil {
		d.SetGate(gate)
	}
	return d
}

// TestP3C_Dispatcher_GatePaused_SuppressesRepairJobs verifies the core P3C
// contract: when the gate is paused, DriftWrongRuntimeState does NOT insert a job.
func TestP3C_Dispatcher_GatePaused_SuppressesRepairJobs(t *testing.T) {
	pool := &p3cFakePool{}
	gate := reconciler.NewRolloutGate()
	gate.Pause("TestP3C suppression test")
	d := makeP3CDispatcher(pool, gate)

	hostID := "host-p3c"
	inst := &db.InstanceRow{
		ID: "inst-p3c-suppress", VMState: "stopping",
		HostID: &hostID, Version: 1,
		UpdatedAt: time.Now().Add(-20 * time.Minute),
	}
	drift := reconciler.DriftResult{
		Class:         reconciler.DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck stopping > 10 min",
	}

	if err := d.Dispatch(context.Background(), inst, drift); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.insertJobCalls != 0 {
		t.Errorf("gate paused: expected 0 job inserts, got %d — suppression broken",
			pool.insertJobCalls)
	}
}

// TestP3C_Dispatcher_GatePaused_DoesNotSuppressFailInstance verifies that
// DriftStuckProvisioning (the failInstance path) is NOT gated.
// State corrections must proceed regardless of rollout gate state.
func TestP3C_Dispatcher_GatePaused_DoesNotSuppressFailInstance(t *testing.T) {
	pool := &p3cFakePool{}
	gate := reconciler.NewRolloutGate()
	gate.Pause("TestP3C failInstance-not-gated test")
	d := makeP3CDispatcher(pool, gate)

	inst := &db.InstanceRow{
		ID: "inst-p3c-fail", VMState: "provisioning",
		HostID: nil, Version: 1,
		UpdatedAt: time.Now().Add(-20 * time.Minute),
	}
	drift := reconciler.DriftResult{
		Class:         reconciler.DriftStuckProvisioning,
		RepairJobType: "",
		Reason:        "stuck provisioning > 15 min",
	}

	if err := d.Dispatch(context.Background(), inst, drift); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// failInstance issues an UPDATE via Exec — verify it was called.
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.execCalls == 0 {
		t.Error("gate paused: failInstance path must NOT be suppressed — Exec must be called")
	}
}

// TestP3C_Dispatcher_GateResumed_AllowsRepairJobs verifies that after Resume,
// repair job dispatch proceeds normally.
func TestP3C_Dispatcher_GateResumed_AllowsRepairJobs(t *testing.T) {
	pool := &p3cFakePool{}
	gate := reconciler.NewRolloutGate()
	gate.Pause("temp")
	gate.Resume()
	d := makeP3CDispatcher(pool, gate)

	hostID := "host-p3c-resumed"
	inst := &db.InstanceRow{
		ID: "inst-p3c-resumed", VMState: "stopping",
		HostID: &hostID, Version: 1,
		UpdatedAt: time.Now().Add(-20 * time.Minute),
	}
	drift := reconciler.DriftResult{
		Class:         reconciler.DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck stopping",
	}

	if err := d.Dispatch(context.Background(), inst, drift); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.execCalls == 0 {
		t.Error("gate resumed: repair job dispatch must proceed — Exec must be called")
	}
}

// TestP3C_Dispatcher_NilGate_AllowsRepairJobs verifies that nil gate (no gate
// wired) allows repair jobs normally — backward compatibility.
func TestP3C_Dispatcher_NilGate_AllowsRepairJobs(t *testing.T) {
	pool := &p3cFakePool{}
	d := makeP3CDispatcher(pool, nil) // no gate

	hostID := "host-p3c-nogate"
	inst := &db.InstanceRow{
		ID: "inst-p3c-nogate", VMState: "stopping",
		HostID: &hostID, Version: 1,
		UpdatedAt: time.Now().Add(-20 * time.Minute),
	}
	drift := reconciler.DriftResult{
		Class:         reconciler.DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck stopping",
	}

	if err := d.Dispatch(context.Background(), inst, drift); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.execCalls == 0 {
		t.Error("nil gate: dispatch must proceed normally")
	}
}

// ── DB-level acceptance (requires DATABASE_URL) ───────────────────────────────

// TestP3C_DB_JobNotWritten_WhenGatePaused verifies at the real DB layer that
// no job row is inserted when the gate is paused and a DriftWrongRuntimeState
// is dispatched.
//
// Environment: requires DATABASE_URL. testRepo() skips if not set.
func TestP3C_DB_JobNotWritten_WhenGatePaused(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t) // skips if DATABASE_URL not set

	// Insert a host + instance to satisfy FK constraints.
	hostID := fmt.Sprintf("p3c-gate-host-%d", time.Now().UnixNano())
	if err := repo.UpsertHost(ctx, &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4, TotalMemoryMB: 8192, TotalDiskGB: 100,
		AgentVersion: "v0.1.0",
	}); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	instanceID := idgen.New(idgen.PrefixInstance)
	if err := repo.InsertInstance(ctx, &db.InstanceRow{
		ID:               instanceID,
		Name:             "p3c-gate-test",
		OwnerPrincipalID: "00000000-0000-0000-0000-000000000001",
		VMState:          "stopping",
		InstanceTypeID:   "c1.small",
		ImageID:          "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
		Version:          0,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("InsertInstance: %v", err)
	}

	// Wire dispatcher with a paused gate.
	gate := reconciler.NewRolloutGate()
	gate.Pause("p3c DB acceptance — verifying job suppression at DB layer")

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	limiter := reconciler.NewRateLimiter()
	d := reconciler.NewDispatcher(repo, limiter, log)
	d.SetGate(gate)

	hid := hostID
	inst := &db.InstanceRow{
		ID:        instanceID,
		VMState:   "stopping",
		HostID:    &hid,
		Version:   0,
		UpdatedAt: time.Now().Add(-20 * time.Minute),
	}
	drift := reconciler.DriftResult{
		Class:         reconciler.DriftWrongRuntimeState,
		RepairJobType: "INSTANCE_STOP",
		Reason:        "stuck stopping (p3c gate DB test)",
	}

	if err := d.Dispatch(ctx, inst, drift); err != nil {
		t.Fatalf("Dispatch with paused gate: %v", err)
	}

	hasJob, err := repo.HasActivePendingJob(ctx, instanceID, "INSTANCE_STOP")
	if err != nil {
		t.Fatalf("HasActivePendingJob: %v", err)
	}
	if hasJob {
		t.Error("DB: job was inserted despite paused rollout gate — suppression broken at DB layer")
	}
	t.Log("PASS: no repair job written to DB while rollout gate was paused")
}
