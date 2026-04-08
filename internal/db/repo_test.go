package db

// repo_test.go — Unit tests for db.Repo using a fake Pool.
//
// Tests cover the SQL logic and argument mapping for all repo methods
// without requiring a real PostgreSQL instance.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ── fakePool ──────────────────────────────────────────────────────────────────
// Implements db.Pool. Records calls and returns scripted responses.

type fakePool struct {
	// execCalls records each Exec call: [query, args...]
	execCalls []execCall
	// execResult controls what Exec returns
	execRows int64
	execErr  error

	// queryRowResult is returned by QueryRow
	queryRowResult fakeRow
	queryRowErr    error

	// queryResult is returned by Query
	queryRowsData [][]any
}

type execCall struct {
	query string
	args  []any
}

func (p *fakePool) Exec(_ context.Context, query string, args ...any) (CommandTag, error) {
	p.execCalls = append(p.execCalls, execCall{query: query, args: args})
	if p.execErr != nil {
		return nil, p.execErr
	}
	return &fakeTag{rows: p.execRows}, nil
}

func (p *fakePool) Query(_ context.Context, _ string, _ ...any) (Rows, error) {
	return &fakeRows{data: p.queryRowsData, idx: -1}, nil
}

func (p *fakePool) QueryRow(_ context.Context, _ string, args ...any) Row {
	if p.queryRowErr != nil {
		return &fakeRow{err: p.queryRowErr}
	}
	return &p.queryRowResult
}

func (p *fakePool) Close() {}

type fakeTag struct{ rows int64 }

func (t *fakeTag) RowsAffected() int64 { return t.rows }

type fakeRow struct {
	values []any
	err    error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if i >= len(r.values) {
			break
		}
		if r.values[i] == nil {
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
		}
	}
	return nil
}

type fakeRows struct {
	data [][]any
	idx  int
}

func (r *fakeRows) Next() bool {
	r.idx++
	return r.idx < len(r.data)
}
func (r *fakeRows) Scan(dest ...any) error {
	if r.idx >= len(r.data) {
		return fmt.Errorf("no row")
	}
	row := r.data[r.idx]
	for i, d := range dest {
		if i >= len(row) || row[i] == nil {
			continue
		}
		switch dst := d.(type) {
		case *string:
			if v, ok := row[i].(string); ok {
				*dst = v
			}
		case *int:
			if v, ok := row[i].(int); ok {
				*dst = v
			}
		case **string:
			if v, ok := row[i].(string); ok {
				*dst = &v
			}
		case *time.Time:
			if v, ok := row[i].(time.Time); ok {
				*dst = v
			}
		case **time.Time:
			if v, ok := row[i].(time.Time); ok {
				*dst = &v
			}
		}
	}
	return nil
}
func (r *fakeRows) Close() {}
func (r *fakeRows) Err() error { return nil }

// ── Helpers ───────────────────────────────────────────────────────────────────

func newRepo(pool *fakePool) *Repo {
	return New(pool)
}

func ctx() context.Context { return context.Background() }

// ── InsertInstance ────────────────────────────────────────────────────────────

func TestInsertInstance_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	now := time.Now()
	row := &InstanceRow{
		ID:               "inst_test001",
		Name:             "my-vm",
		OwnerPrincipalID: "princ_001",
		InstanceTypeID:   "c1.small",
		ImageID:          "img-001",
		AvailabilityZone: "us-east-1a",
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := r.InsertInstance(ctx(), row); err != nil {
		t.Fatalf("InsertInstance: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	// Verify the instance ID is passed as first arg.
	if len(call.args) == 0 || call.args[0] != "inst_test001" {
		t.Errorf("first arg = %v, want inst_test001", call.args[0])
	}
}

func TestInsertInstance_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db error")}
	r := newRepo(pool)

	err := r.InsertInstance(ctx(), &InstanceRow{ID: "x"})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// ── GetInstanceByID ───────────────────────────────────────────────────────────

func TestGetInstanceByID_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"inst_001", "my-vm", "princ_001", "running",
			"c1.small", "img-001", nil, "us-east-1a",
			0, now, now, nil,
		}},
	}
	r := newRepo(pool)

	inst, err := r.GetInstanceByID(ctx(), "inst_001")
	if err != nil {
		t.Fatalf("GetInstanceByID: %v", err)
	}
	if inst.ID != "inst_001" {
		t.Errorf("ID = %q, want inst_001", inst.ID)
	}
	if inst.VMState != "running" {
		t.Errorf("VMState = %q, want running", inst.VMState)
	}
}

func TestGetInstanceByID_ReturnsError_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.GetInstanceByID(ctx(), "inst_missing")
	if err == nil {
		t.Error("expected error for missing instance, got nil")
	}
}

// ── UpdateInstanceState ───────────────────────────────────────────────────────

func TestUpdateInstanceState_Success_WhenVersionAndStateMatch(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.UpdateInstanceState(ctx(), "inst_001", "requested", "provisioning", 0); err != nil {
		t.Fatalf("UpdateInstanceState: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
}

func TestUpdateInstanceState_ReturnsError_WhenZeroRowsAffected(t *testing.T) {
	pool := &fakePool{execRows: 0} // 0 rows = version/state mismatch
	r := newRepo(pool)

	err := r.UpdateInstanceState(ctx(), "inst_001", "requested", "provisioning", 99)
	if err == nil {
		t.Error("expected error for 0 rows affected (optimistic lock), got nil")
	}
}

// ── AssignHost ────────────────────────────────────────────────────────────────

func TestAssignHost_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.AssignHost(ctx(), "inst_001", "host_001", 0); err != nil {
		t.Fatalf("AssignHost: %v", err)
	}
	call := pool.execCalls[0]
	// host_id should be the second arg
	if len(call.args) < 2 || call.args[1] != "host_001" {
		t.Errorf("host_id arg = %v, want host_001", call.args)
	}
}

func TestAssignHost_ReturnsError_WhenZeroRowsAffected(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.AssignHost(ctx(), "inst_001", "host_001", 99)
	if err == nil {
		t.Error("expected error for 0 rows affected, got nil")
	}
}

// ── SoftDeleteInstance ────────────────────────────────────────────────────────

func TestSoftDeleteInstance_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.SoftDeleteInstance(ctx(), "inst_001", 2); err != nil {
		t.Fatalf("SoftDeleteInstance: %v", err)
	}
}

func TestSoftDeleteInstance_ReturnsError_WhenVersionMismatch(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.SoftDeleteInstance(ctx(), "inst_001", 99)
	if err == nil {
		t.Error("expected error for stale version, got nil")
	}
}

// ── InsertEvent ───────────────────────────────────────────────────────────────

func TestInsertEvent_CallsExec(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	err := r.InsertEvent(ctx(), &EventRow{
		ID:         "evt_001",
		InstanceID: "inst_001",
		EventType:  EventInstanceCreate,
		Message:    "test event",
		Actor:      "system",
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
}

// ── InsertJob ─────────────────────────────────────────────────────────────────

func TestInsertJob_CallsExecWithJobID(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	err := r.InsertJob(ctx(), &JobRow{
		ID:             "job_001",
		InstanceID:     "inst_001",
		JobType:        "INSTANCE_CREATE",
		IdempotencyKey: "idem_001",
		MaxAttempts:    3,
	})
	if err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	call := pool.execCalls[0]
	if call.args[0] != "job_001" {
		t.Errorf("first arg = %v, want job_001", call.args[0])
	}
}

// ── UpdateJobStatus ───────────────────────────────────────────────────────────

func TestUpdateJobStatus_Completed_NoErrorMessage(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.UpdateJobStatus(ctx(), "job_001", "completed", nil); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
}

func TestUpdateJobStatus_Failed_WithErrorMessage(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	msg := "something went wrong"
	if err := r.UpdateJobStatus(ctx(), "job_001", "failed", &msg); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	call := pool.execCalls[0]
	// status is first arg, errMsg is second
	if call.args[0] != "job_001" {
		t.Errorf("first arg = %v, want job_001", call.args[0])
	}
}

func TestUpdateJobStatus_ReturnsError_WhenJobNotFound(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.UpdateJobStatus(ctx(), "job_missing", "completed", nil)
	if err == nil {
		t.Error("expected error when job not found (0 rows affected), got nil")
	}
}

// ── AllocateIP ────────────────────────────────────────────────────────────────

func TestAllocateIP_ReturnsIP_OnSuccess(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{"10.0.0.5"}},
	}
	r := newRepo(pool)

	ip, err := r.AllocateIP(ctx(), "vpc_001", "inst_001")
	if err != nil {
		t.Fatalf("AllocateIP: %v", err)
	}
	if ip != "10.0.0.5" {
		t.Errorf("ip = %q, want 10.0.0.5", ip)
	}
}

func TestAllocateIP_ReturnsError_WhenPoolExhausted(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.AllocateIP(ctx(), "vpc_empty", "inst_001")
	if err == nil {
		t.Error("expected error when IP pool is exhausted, got nil")
	}
}

// ── ReleaseIP ─────────────────────────────────────────────────────────────────

func TestReleaseIP_Success_IsIdempotent(t *testing.T) {
	// 0 rows affected is fine — already released.
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	if err := r.ReleaseIP(ctx(), "10.0.0.5", "vpc_001", "inst_001"); err != nil {
		t.Fatalf("ReleaseIP: %v", err)
	}
}

func TestReleaseIP_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("connection lost")}
	r := newRepo(pool)

	err := r.ReleaseIP(ctx(), "10.0.0.5", "vpc_001", "inst_001")
	if err == nil {
		t.Error("expected error from Exec, got nil")
	}
}

// ── GetIPByInstance ───────────────────────────────────────────────────────────

func TestGetIPByInstance_ReturnsIP(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{"10.0.0.7"}},
	}
	r := newRepo(pool)

	ip, err := r.GetIPByInstance(ctx(), "inst_001")
	if err != nil {
		t.Fatalf("GetIPByInstance: %v", err)
	}
	if ip != "10.0.0.7" {
		t.Errorf("ip = %q, want 10.0.0.7", ip)
	}
}

func TestGetIPByInstance_ReturnsEmptyString_WhenNotAllocated(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	ip, err := r.GetIPByInstance(ctx(), "inst_noip")
	if err != nil {
		t.Fatalf("GetIPByInstance: %v", err)
	}
	if ip != "" {
		t.Errorf("ip = %q, want empty string (not allocated)", ip)
	}
}

// ── Event type constants ──────────────────────────────────────────────────────

func TestEventTypeConstants_AllDefined(t *testing.T) {
	constants := []string{
		EventInstanceCreate,
		EventInstanceProvisioningStart,
		EventInstanceProvisioningDone,
		EventInstanceStart,
		EventInstanceStop,
		EventInstanceDeleteInitiate,
		EventInstanceDelete,
		EventInstanceFailure,
		EventUsageStart,
		EventUsageEnd,
		EventIPAllocated,
		EventIPReleased,
	}
	for _, c := range constants {
		if c == "" {
			t.Errorf("event type constant is empty string")
		}
	}
}

// ── DatabaseURL ───────────────────────────────────────────────────────────────

func TestDatabaseURL_PanicsWhenUnset(t *testing.T) {
	// Ensure DATABASE_URL is not set in this test.
	t.Setenv("DATABASE_URL", "")

	defer func() {
		if r := recover(); r == nil {
			t.Error("DatabaseURL() should panic when DATABASE_URL is unset")
		}
	}()
	DatabaseURL()
}

func TestDatabaseURL_ReturnsValue_WhenSet(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	url := DatabaseURL()
	if url == "" {
		t.Error("DatabaseURL returned empty string when env is set")
	}
}
