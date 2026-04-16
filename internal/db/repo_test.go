package db

// repo_test.go — Unit tests for db.Repo using a fake Pool.
//
// Tests cover the SQL logic and argument mapping for all repo methods
// without requiring a real PostgreSQL instance.
//
// VM-P2E Slice 2 additions (search "VM-P2E Slice 2"):
//   - TestUpdateHostStatus_*
//   - TestMarkHostDrained_*
//   - TestDetachStoppedInstancesFromHost_*
//   - TestCountActiveInstancesOnHost_*
//   - TestGetHostByID_ScansGenerationAndDrainReason
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ── fakePool ──────────────────────────────────────────────────────────────────

type fakePool struct {
	execCalls []execCall
	execRows  int64
	execErr   error

	queryRowResult fakeRow
	queryRowErr    error

	// multiQueryRow: if set, QueryRow returns these results in order.
	// Used when a single test drives multiple QueryRow calls (e.g. MarkHostDrained).
	multiQueryRow []fakeRow
	multiIdx      int

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
	if p.multiQueryRow != nil {
		if p.multiIdx < len(p.multiQueryRow) {
			r := p.multiQueryRow[p.multiIdx]
			p.multiIdx++
			return &r
		}
		return &fakeRow{err: fmt.Errorf("no more scripted rows")}
	}
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
			switch v := r.values[i].(type) {
			case int:
				*dst = v
			case int64:
				*dst = int(v)
			}
		case *int64:
			switch v := r.values[i].(type) {
			case int64:
				*dst = v
			case int:
				*dst = int64(v)
			}
		case *bool:
			switch v := r.values[i].(type) {
			case bool:
				*dst = v
			case int:
				*dst = v != 0
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
			switch v := row[i].(type) {
			case int:
				*dst = v
			case int64:
				*dst = int(v)
			}
		case *int64:
			switch v := row[i].(type) {
			case int64:
				*dst = v
			case int:
				*dst = int64(v)
			}
		case *bool:
			switch v := row[i].(type) {
			case bool:
				*dst = v
			case int:
				*dst = v != 0
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
	pool := &fakePool{execRows: 0}
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

// ── VM-P2E Slice 2: UpdateHostStatus ─────────────────────────────────────────

func TestUpdateHostStatus_ReturnsTrue_WhenCASSucceeds(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	ok, err := r.UpdateHostStatus(ctx(), "host_001", 0, "draining", "kernel upgrade")
	if err != nil {
		t.Fatalf("UpdateHostStatus: %v", err)
	}
	if !ok {
		t.Error("expected updated=true when 1 row affected")
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	// args: hostID=$1, newStatus=$2, drainReason=$3, fromGeneration=$4
	if call.args[0] != "host_001" {
		t.Errorf("arg[0] = %v, want host_001", call.args[0])
	}
	if call.args[1] != "draining" {
		t.Errorf("arg[1] = %v, want draining", call.args[1])
	}
	if call.args[3] != int64(0) {
		t.Errorf("arg[3] = %v, want int64(0)", call.args[3])
	}
}

func TestUpdateHostStatus_ReturnsFalse_WhenCASFails(t *testing.T) {
	pool := &fakePool{execRows: 0} // generation mismatch → 0 rows
	r := newRepo(pool)

	ok, err := r.UpdateHostStatus(ctx(), "host_001", 5, "draining", "")
	if err != nil {
		t.Fatalf("UpdateHostStatus: %v", err)
	}
	if ok {
		t.Error("expected updated=false when 0 rows affected (generation mismatch)")
	}
}

func TestUpdateHostStatus_EmptyDrainReason_StoresNULL(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	_, err := r.UpdateHostStatus(ctx(), "host_001", 0, "ready", "")
	if err != nil {
		t.Fatalf("UpdateHostStatus: %v", err)
	}
	call := pool.execCalls[0]
	// drainReason arg should be nil (SQL NULL) when empty string is passed.
	if call.args[2] != nil {
		t.Errorf("expected drain_reason arg to be nil for empty string, got %v", call.args[2])
	}
}

func TestUpdateHostStatus_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db down")}
	r := newRepo(pool)

	_, err := r.UpdateHostStatus(ctx(), "host_001", 0, "draining", "")
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// ── VM-P2E Slice 2: MarkHostDrained ──────────────────────────────────────────

func TestMarkHostDrained_TransitionSucceeds_WhenNoActiveInstances(t *testing.T) {
	// CountActiveInstancesOnHost → 0 rows; MarkHostDrained CAS → 1 row.
	pool := &fakePool{
		multiQueryRow: []fakeRow{
			{values: []any{0}}, // COUNT(*) = 0
		},
		execRows: 1, // CAS UPDATE affected 1 row
	}
	r := newRepo(pool)

	activeCount, updated, err := r.MarkHostDrained(ctx(), "host_001", 3)
	if err != nil {
		t.Fatalf("MarkHostDrained: %v", err)
	}
	if activeCount != 0 {
		t.Errorf("activeCount = %d, want 0", activeCount)
	}
	if !updated {
		t.Error("expected updated=true when CAS succeeds with 0 active instances")
	}
	// Should have issued exactly 1 Exec (the UPDATE) after the count.
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec (CAS UPDATE), got %d", len(pool.execCalls))
	}
}

func TestMarkHostDrained_BlockedByActiveInstances(t *testing.T) {
	pool := &fakePool{
		multiQueryRow: []fakeRow{
			{values: []any{3}}, // COUNT(*) = 3 active instances
		},
	}
	r := newRepo(pool)

	activeCount, updated, err := r.MarkHostDrained(ctx(), "host_001", 2)
	if err != nil {
		t.Fatalf("MarkHostDrained: %v", err)
	}
	if activeCount != 3 {
		t.Errorf("activeCount = %d, want 3", activeCount)
	}
	if updated {
		t.Error("expected updated=false when active instances remain")
	}
	// No Exec should have been issued when count > 0.
	if len(pool.execCalls) != 0 {
		t.Errorf("expected 0 Exec calls when blocked by active instances, got %d", len(pool.execCalls))
	}
}

func TestMarkHostDrained_Idempotent_WhenAlreadyDrained(t *testing.T) {
	// COUNT(*) = 0 but the CAS UPDATE matches 0 rows (status != 'draining').
	pool := &fakePool{
		multiQueryRow: []fakeRow{
			{values: []any{0}},
		},
		execRows: 0, // host already drained or wrong generation
	}
	r := newRepo(pool)

	activeCount, updated, err := r.MarkHostDrained(ctx(), "host_001", 5)
	if err != nil {
		t.Fatalf("MarkHostDrained: %v", err)
	}
	if activeCount != 0 {
		t.Errorf("activeCount = %d, want 0", activeCount)
	}
	if updated {
		t.Error("expected updated=false when CAS matches 0 rows (already drained or wrong gen)")
	}
}

func TestMarkHostDrained_PropagatesCountError(t *testing.T) {
	pool := &fakePool{
		multiQueryRow: []fakeRow{
			{err: fmt.Errorf("db error during count")},
		},
	}
	r := newRepo(pool)

	_, _, err := r.MarkHostDrained(ctx(), "host_001", 0)
	if err == nil {
		t.Error("expected error when count query fails")
	}
}

func TestMarkHostDrained_PropagatesUpdateError(t *testing.T) {
	pool := &fakePool{
		multiQueryRow: []fakeRow{
			{values: []any{0}}, // count = 0 → proceed to CAS
		},
		execErr: fmt.Errorf("db error during update"),
	}
	r := newRepo(pool)

	_, _, err := r.MarkHostDrained(ctx(), "host_001", 0)
	if err == nil {
		t.Error("expected error when CAS UPDATE fails")
	}
}

// ── VM-P2E Slice 2: DetachStoppedInstancesFromHost ───────────────────────────

func TestDetachStoppedInstancesFromHost_CallsExecWithHostID(t *testing.T) {
	pool := &fakePool{execRows: 2} // 2 stopped instances detached
	r := newRepo(pool)

	if err := r.DetachStoppedInstancesFromHost(ctx(), "host_001"); err != nil {
		t.Fatalf("DetachStoppedInstancesFromHost: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "host_001" {
		t.Errorf("arg[0] = %v, want host_001", call.args[0])
	}
}

func TestDetachStoppedInstancesFromHost_Idempotent_WhenNoStoppedInstances(t *testing.T) {
	pool := &fakePool{execRows: 0} // no stopped instances → 0 rows affected, still OK
	r := newRepo(pool)

	if err := r.DetachStoppedInstancesFromHost(ctx(), "host_empty"); err != nil {
		t.Fatalf("DetachStoppedInstancesFromHost with 0 matches should not error: %v", err)
	}
}

func TestDetachStoppedInstancesFromHost_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db gone")}
	r := newRepo(pool)

	err := r.DetachStoppedInstancesFromHost(ctx(), "host_001")
	if err == nil {
		t.Error("expected error from Exec, got nil")
	}
}

// ── VM-P2E Slice 2: CountActiveInstancesOnHost ───────────────────────────────

func TestCountActiveInstancesOnHost_ReturnsCount(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{4}},
	}
	r := newRepo(pool)

	n, err := r.CountActiveInstancesOnHost(ctx(), "host_001")
	if err != nil {
		t.Fatalf("CountActiveInstancesOnHost: %v", err)
	}
	if n != 4 {
		t.Errorf("n = %d, want 4", n)
	}
}

func TestCountActiveInstancesOnHost_ReturnsZero_WhenEmpty(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{0}},
	}
	r := newRepo(pool)

	n, err := r.CountActiveInstancesOnHost(ctx(), "host_empty")
	if err != nil {
		t.Fatalf("CountActiveInstancesOnHost: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

// ── VM-P2E Slice 2: GetHostByID scans generation and drain_reason ─────────────

func TestGetHostByID_ScansGenerationAndDrainReason(t *testing.T) {
	now := time.Now()
	reason := "kernel upgrade"
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"host_001",   // id
			"us-east-1a", // availability_zone
			"draining",   // status
			int64(3),     // generation
			reason,       // drain_reason (non-nil)
			16,           // total_cpu
			65536,        // total_memory_mb
			500,          // total_disk_gb
			4,            // used_cpu
			16384,        // used_memory_mb
			100,          // used_disk_gb
			"v1.2.3",     // agent_version
			now,          // last_heartbeat_at
			now,          // registered_at
			now,          // updated_at
		}},
	}
	r := newRepo(pool)

	h, err := r.GetHostByID(ctx(), "host_001")
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if h.Status != "draining" {
		t.Errorf("Status = %q, want draining", h.Status)
	}
	if h.Generation != 3 {
		t.Errorf("Generation = %d, want 3", h.Generation)
	}
	if h.DrainReason == nil || *h.DrainReason != "kernel upgrade" {
		t.Errorf("DrainReason = %v, want \"kernel upgrade\"", h.DrainReason)
	}
}

func TestGetHostByID_DrainReasonNil_WhenNotSet(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"host_002", "us-east-1a", "ready",
			int64(0), nil, // generation=0, drain_reason=NULL
			8, 32768, 200,
			0, 0, 0,
			"v1.0.0", now, now, now,
		}},
	}
	r := newRepo(pool)

	h, err := r.GetHostByID(ctx(), "host_002")
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if h.DrainReason != nil {
		t.Errorf("DrainReason = %v, want nil", h.DrainReason)
	}
	if h.Generation != 0 {
		t.Errorf("Generation = %d, want 0", h.Generation)
	}
}

func TestGetHostByID_ReturnsErrHostNotFound_WhenAbsent(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.GetHostByID(ctx(), "host_missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isHostNotFound(err) {
		t.Errorf("expected ErrHostNotFound in error chain, got: %v", err)
	}
}

// isHostNotFound checks whether ErrHostNotFound appears in the error string,
// matching the fmt.Errorf wrapping done by GetHostByID.
func isHostNotFound(err error) bool {
	return err != nil && (err == ErrHostNotFound ||
		containsString(err.Error(), ErrHostNotFound.Error()))
}

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		len(s) > 0 && findSubstr(s, sub))
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
