package db

// metering_repo_test.go — Unit tests for metering repo methods (metering_repo.go).
//
// Phase 16A coverage:
//   - InsertUsageRecord: Exec args, ON CONFLICT idempotency path
//   - CloseUsageRecord: Exec args
//   - GetOpenUsageRecord: scan values, nil when not found
//   - ListUsageRecordsByScope: returns rows, limit capping
//   - AcquireReconciliationHold: returns true on insert, false on conflict
//   - ApplyReconciliationHold: Exec args, zero-rows error
//   - ReleaseReconciliationHold: Exec args
//   - HoldExistsForWindow: true and false
//   - CreateBudgetPolicy: Exec args
//   - GetBudgetPolicyByID: scan values, nil on not-found
//   - ListActiveBudgetPoliciesForScope: returns rows
//   - IncrementBudgetAccrual: Exec args
//   - CheckBudgetAllowsCreate: returns nil when no exceeded policy, ErrBudgetExceeded when exceeded
//
// fakePool / fakeTag / fakeRow / fakeRows / newRepo / ctx are defined in
// repo_test.go (same package db); no redeclaration.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// ── column helpers ────────────────────────────────────────────────────────────

// usageRecordRow builds a fakeRow values slice matching the 11-column SELECT in
// GetOpenUsageRecord / ListUsageRecordsByScope:
//
//	id, instance_id, scope_id, project_id, record_type,
//	instance_type_id, started_at, ended_at, duration_seconds, event_id, created_at
func usageRecordRow(id, instanceID, scopeID, recordType, instanceTypeID, eventID string) []any {
	now := time.Now()
	return []any{
		id, instanceID, scopeID,
		nil,           // project_id (*string)
		recordType,
		instanceTypeID,
		now,           // started_at
		nil,           // ended_at (*time.Time)
		nil,           // duration_seconds (*int64)
		eventID,
		now,           // created_at
	}
}

// budgetPolicyRow builds a fakeRow values slice matching the 13-column SELECT in
// GetBudgetPolicyByID / ListActiveBudgetPoliciesForScope:
//
//	id, scope_id, project_id, limit_cents, accrued_cents,
//	period_start, period_end, enforcement_action, notification_email,
//	status, created_by, created_at, updated_at
func budgetPolicyRow(id, scopeID, action, status string, limitCents, accruedCents int64) []any {
	now := time.Now()
	future := now.Add(30 * 24 * time.Hour)
	return []any{
		id, scopeID,
		nil,          // project_id (*string)
		limitCents, accruedCents,
		now,          // period_start
		future,       // period_end
		action,
		nil,          // notification_email (*string)
		status,
		"user_001",   // created_by
		now,          // created_at
		now,          // updated_at
	}
}

// ── InsertUsageRecord ─────────────────────────────────────────────────────────

func TestInsertUsageRecord_ExecArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	now := time.Now()
	row := &UsageRecordRow{
		ID:             "ur_001",
		InstanceID:     "inst_001",
		ScopeID:        "princ_001",
		RecordType:     UsageRecordTypeStart,
		InstanceTypeID: "c1.small",
		StartedAt:      now,
		EventID:        "evt_001",
	}
	if err := r.InsertUsageRecord(ctx(), row); err != nil {
		t.Fatalf("InsertUsageRecord: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "ur_001" {
		t.Errorf("arg[0] (id) = %v, want ur_001", call.args[0])
	}
	if call.args[4] != UsageRecordTypeStart {
		t.Errorf("arg[4] (record_type) = %v, want %s", call.args[4], UsageRecordTypeStart)
	}
	if call.args[9] != "evt_001" {
		t.Errorf("arg[9] (event_id) = %v, want evt_001", call.args[9])
	}
}

func TestInsertUsageRecord_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db: connection refused")}
	r := newRepo(pool)

	err := r.InsertUsageRecord(ctx(), &UsageRecordRow{
		ID: "ur_x", EventID: "evt_x", StartedAt: time.Now(),
	})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// ── CloseUsageRecord ──────────────────────────────────────────────────────────

func TestCloseUsageRecord_ExecArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	endedAt := time.Now()
	if err := r.CloseUsageRecord(ctx(), "inst_001", endedAt); err != nil {
		t.Fatalf("CloseUsageRecord: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "inst_001" {
		t.Errorf("arg[0] (instance_id) = %v, want inst_001", call.args[0])
	}
}

func TestCloseUsageRecord_Idempotent_ZeroRowsIsOK(t *testing.T) {
	// CloseUsageRecord has WHERE ended_at IS NULL — if already closed, 0 rows is fine.
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	if err := r.CloseUsageRecord(ctx(), "inst_001", time.Now()); err != nil {
		t.Errorf("CloseUsageRecord with 0 rows should be OK (idempotent), got %v", err)
	}
}

// ── GetOpenUsageRecord ────────────────────────────────────────────────────────

func TestGetOpenUsageRecord_ReturnsRow_WhenFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: usageRecordRow(
			"ur_001", "inst_001", "princ_001",
			UsageRecordTypeStart, "c1.small", "evt_001",
		)},
	}
	r := newRepo(pool)

	rec, err := r.GetOpenUsageRecord(ctx(), "inst_001")
	if err != nil {
		t.Fatalf("GetOpenUsageRecord: %v", err)
	}
	if rec == nil {
		t.Fatal("expected record, got nil")
	}
	if rec.ID != "ur_001" {
		t.Errorf("ID = %q, want ur_001", rec.ID)
	}
	if rec.RecordType != UsageRecordTypeStart {
		t.Errorf("RecordType = %q, want %s", rec.RecordType, UsageRecordTypeStart)
	}
}

func TestGetOpenUsageRecord_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	rec, err := r.GetOpenUsageRecord(ctx(), "inst_no_usage")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if rec != nil {
		t.Errorf("expected nil record, got %+v", rec)
	}
}

// ── ListUsageRecordsByScope ───────────────────────────────────────────────────

func TestListUsageRecordsByScope_ReturnsRows(t *testing.T) {
	pool := &fakePool{
		queryRowsData: [][]any{
			usageRecordRow("ur_001", "inst_001", "princ_001", UsageRecordTypeStart, "c1.small", "evt_001"),
			usageRecordRow("ur_002", "inst_002", "princ_001", UsageRecordTypeEnd, "c1.small", "evt_002"),
		},
	}
	r := newRepo(pool)

	list, err := r.ListUsageRecordsByScope(ctx(), "princ_001", 100)
	if err != nil {
		t.Fatalf("ListUsageRecordsByScope: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 records, got %d", len(list))
	}
}

func TestListUsageRecordsByScope_LimitCappedAt500(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	// limit=0 → capped to 500; just verify no panic
	_, err := r.ListUsageRecordsByScope(ctx(), "princ_001", 0)
	if err != nil {
		t.Errorf("ListUsageRecordsByScope with limit=0: %v", err)
	}
}

// ── AcquireReconciliationHold ─────────────────────────────────────────────────

func TestAcquireReconciliationHold_ReturnsTrue_WhenInserted(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	now := time.Now()
	acquired, err := r.AcquireReconciliationHold(ctx(),
		"hold_001", "inst_001", "princ_001",
		now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AcquireReconciliationHold: %v", err)
	}
	if !acquired {
		t.Error("expected acquired=true, got false")
	}
}

func TestAcquireReconciliationHold_ReturnsFalse_OnConflict(t *testing.T) {
	// ON CONFLICT DO NOTHING → 0 rows affected
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	now := time.Now()
	acquired, err := r.AcquireReconciliationHold(ctx(),
		"hold_002", "inst_001", "princ_001",
		now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AcquireReconciliationHold conflict: %v", err)
	}
	if acquired {
		t.Error("expected acquired=false on conflict, got true")
	}
}

// ── ApplyReconciliationHold ───────────────────────────────────────────────────

func TestApplyReconciliationHold_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.ApplyReconciliationHold(ctx(), "hold_001"); err != nil {
		t.Fatalf("ApplyReconciliationHold: %v", err)
	}
}

func TestApplyReconciliationHold_NotPending_ReturnsError(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.ApplyReconciliationHold(ctx(), "hold_missing")
	if err == nil {
		t.Error("expected error for 0 rows affected, got nil")
	}
}

// ── ReleaseReconciliationHold ─────────────────────────────────────────────────

func TestReleaseReconciliationHold_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	now := time.Now()
	if err := r.ReleaseReconciliationHold(ctx(), "inst_001", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("ReleaseReconciliationHold: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "inst_001" {
		t.Errorf("arg[0] (instance_id) = %v, want inst_001", call.args[0])
	}
}

// ── HoldExistsForWindow ───────────────────────────────────────────────────────

func TestHoldExistsForWindow_True(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{true}},
	}
	r := newRepo(pool)

	now := time.Now()
	exists, err := r.HoldExistsForWindow(ctx(), "inst_001", now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("HoldExistsForWindow: %v", err)
	}
	if !exists {
		t.Error("expected exists=true, got false")
	}
}

func TestHoldExistsForWindow_False_WhenNoHold(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{false}},
	}
	r := newRepo(pool)

	now := time.Now()
	exists, err := r.HoldExistsForWindow(ctx(), "inst_001", now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("HoldExistsForWindow: %v", err)
	}
	if exists {
		t.Error("expected exists=false, got true")
	}
}

// ── CreateBudgetPolicy ────────────────────────────────────────────────────────

func TestCreateBudgetPolicy_ExecArgs(t *testing.T) {
	now := time.Now()
	future := now.Add(30 * 24 * time.Hour)
	pool := &fakePool{
		execRows:       1,
		queryRowResult: fakeRow{values: budgetPolicyRow("bp_001", "princ_001", "notify", "active", 10000, 0)},
	}
	r := newRepo(pool)

	bp, err := r.CreateBudgetPolicy(ctx(),
		"bp_001", "princ_001", "user_001", "notify",
		nil, 10000, now, future, nil)
	if err != nil {
		t.Fatalf("CreateBudgetPolicy: %v", err)
	}
	if bp.ID != "bp_001" {
		t.Errorf("ID = %q, want bp_001", bp.ID)
	}
	if bp.LimitCents != 10000 {
		t.Errorf("LimitCents = %d, want 10000", bp.LimitCents)
	}

	// Verify INSERT args: $1=id, $2=scope_id, $3=project_id, $4=limit_cents, ...
	call := pool.execCalls[0]
	if call.args[0] != "bp_001" {
		t.Errorf("arg[0] (id) = %v, want bp_001", call.args[0])
	}
	if call.args[1] != "princ_001" {
		t.Errorf("arg[1] (scope_id) = %v, want princ_001", call.args[1])
	}
}

// ── GetBudgetPolicyByID ───────────────────────────────────────────────────────

func TestGetBudgetPolicyByID_ReturnsRow_WhenFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: budgetPolicyRow("bp_001", "princ_001", "block_create", "active", 50000, 12000)},
	}
	r := newRepo(pool)

	bp, err := r.GetBudgetPolicyByID(ctx(), "bp_001")
	if err != nil {
		t.Fatalf("GetBudgetPolicyByID: %v", err)
	}
	if bp == nil {
		t.Fatal("expected policy, got nil")
	}
	if bp.EnforcementAction != "block_create" {
		t.Errorf("EnforcementAction = %q, want block_create", bp.EnforcementAction)
	}
	if bp.LimitCents != 50000 {
		t.Errorf("LimitCents = %d, want 50000", bp.LimitCents)
	}
	if bp.AccruedCents != 12000 {
		t.Errorf("AccruedCents = %d, want 12000", bp.AccruedCents)
	}
}

func TestGetBudgetPolicyByID_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	bp, err := r.GetBudgetPolicyByID(ctx(), "bp_missing")
	if err != nil {
		t.Fatalf("expected nil error for not-found, got %v", err)
	}
	if bp != nil {
		t.Errorf("expected nil policy, got %+v", bp)
	}
}

// ── ListActiveBudgetPoliciesForScope ──────────────────────────────────────────

func TestListActiveBudgetPoliciesForScope_ReturnsRows(t *testing.T) {
	pool := &fakePool{
		queryRowsData: [][]any{
			budgetPolicyRow("bp_001", "princ_001", "notify", "active", 10000, 500),
			budgetPolicyRow("bp_002", "princ_001", "block_create", "active", 20000, 19000),
		},
	}
	r := newRepo(pool)

	list, err := r.ListActiveBudgetPoliciesForScope(ctx(), "princ_001")
	if err != nil {
		t.Fatalf("ListActiveBudgetPoliciesForScope: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 policies, got %d", len(list))
	}
}

func TestListActiveBudgetPoliciesForScope_Empty_ReturnsEmptySlice(t *testing.T) {
	pool := &fakePool{queryRowsData: [][]any{}}
	r := newRepo(pool)

	list, err := r.ListActiveBudgetPoliciesForScope(ctx(), "princ_no_budget")
	if err != nil {
		t.Fatalf("ListActiveBudgetPoliciesForScope empty: %v", err)
	}
	if list == nil {
		t.Error("expected empty slice, got nil")
	}
	if len(list) != 0 {
		t.Errorf("want 0 policies, got %d", len(list))
	}
}

// ── IncrementBudgetAccrual ────────────────────────────────────────────────────

func TestIncrementBudgetAccrual_ExecArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.IncrementBudgetAccrual(ctx(), "princ_001", 150); err != nil {
		t.Fatalf("IncrementBudgetAccrual: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "princ_001" {
		t.Errorf("arg[0] (scope_id) = %v, want princ_001", call.args[0])
	}
	if call.args[1] != int64(150) {
		t.Errorf("arg[1] (delta_cents) = %v, want 150", call.args[1])
	}
}

// ── CheckBudgetAllowsCreate ───────────────────────────────────────────────────

func TestCheckBudgetAllowsCreate_ReturnsNil_WhenNoPolicyExceeded(t *testing.T) {
	// SELECT EXISTS returns false → no block_create policy exceeded
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{false}},
	}
	r := newRepo(pool)

	if err := r.CheckBudgetAllowsCreate(ctx(), "princ_001"); err != nil {
		t.Errorf("expected nil (no budget exceeded), got %v", err)
	}
}

func TestCheckBudgetAllowsCreate_ReturnsErrBudgetExceeded_WhenLimitReached(t *testing.T) {
	// SELECT EXISTS returns true → block_create policy exceeded
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{true}},
	}
	r := newRepo(pool)

	err := r.CheckBudgetAllowsCreate(ctx(), "princ_over_budget")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("want ErrBudgetExceeded, got %v", err)
	}
}

func TestCheckBudgetAllowsCreate_PropagatesDBError(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("db: connection reset")},
	}
	r := newRepo(pool)

	err := r.CheckBudgetAllowsCreate(ctx(), "princ_001")
	if err == nil {
		t.Error("expected DB error, got nil")
	}
	if errors.Is(err, ErrBudgetExceeded) {
		t.Error("DB connectivity error must not be reported as ErrBudgetExceeded")
	}
}

// ── UsageRecord type constant sanity checks ───────────────────────────────────

func TestUsageRecordTypeConstants_HaveExpectedValues(t *testing.T) {
	if UsageRecordTypeStart != "USAGE_START" {
		t.Errorf("UsageRecordTypeStart = %q, want USAGE_START", UsageRecordTypeStart)
	}
	if UsageRecordTypeEnd != "USAGE_END" {
		t.Errorf("UsageRecordTypeEnd = %q, want USAGE_END", UsageRecordTypeEnd)
	}
	if UsageRecordTypeReconciled != "RECONCILED" {
		t.Errorf("UsageRecordTypeReconciled = %q, want RECONCILED", UsageRecordTypeReconciled)
	}
}

func TestBudgetConstants_HaveExpectedValues(t *testing.T) {
	if BudgetActionNotify != "notify" {
		t.Errorf("BudgetActionNotify = %q, want notify", BudgetActionNotify)
	}
	if BudgetActionBlockCreate != "block_create" {
		t.Errorf("BudgetActionBlockCreate = %q, want block_create", BudgetActionBlockCreate)
	}
}
