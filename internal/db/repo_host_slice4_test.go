package db

// repo_host_slice4_test.go — Unit tests for VM-P2E Slice 4 host lifecycle DB methods.
//
// Tests cover:
//   - ValidateHostTransition: new legal pairs (Slice 4 retirement paths)
//   - ValidateHostTransition: new illegal pairs (retiring/retired terminal rules)
//   - MarkHostRetiring: CAS success with zero workload
//   - MarkHostRetiring: blocked when active workload remains
//   - MarkHostRetiring: CAS failure (generation mismatch)
//   - MarkHostRetiring: illegal transition
//   - MarkHostRetiring: empty reason code stores NULL
//   - MarkHostRetiring: propagates Exec error
//   - MarkHostRetired: CAS success, sets retired_at
//   - MarkHostRetired: CAS failure (wrong generation or not in retiring)
//   - MarkHostRetired: propagates Exec error
//   - GetRetiredHosts: returns empty slice when none exist
//   - ReasonOperatorRetired: constant is defined and non-empty
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"errors"
	"fmt"
	"testing"
)

// ── ValidateHostTransition: Slice 4 legal pairs ───────────────────────────────

func TestValidateHostTransition_Slice4_LegalPairs(t *testing.T) {
	cases := []struct {
		from, to string
	}{
		// Normal retirement path: drain first, then retire.
		{"drained", "retiring"},
		// Retirement completes.
		{"retiring", "retired"},
		// Direct admin-only shortcut (drained host confirmed unrecoverable).
		{"drained", "retired"},
		// Fenced-then-retire seam for Slice 5 fencing controller.
		{"fenced", "retiring"},
		// Emergency retirement of an unhealthy confirmed-bad host.
		{"unhealthy", "retiring"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			if err := ValidateHostTransition(tc.from, tc.to); err != nil {
				t.Errorf("expected no error for %s→%s, got: %v", tc.from, tc.to, err)
			}
		})
	}
}

// ── ValidateHostTransition: Slice 4 illegal pairs ────────────────────────────

func TestValidateHostTransition_Slice4_IllegalPairs(t *testing.T) {
	cases := []struct {
		from, to string
		desc     string
	}{
		// retired is terminal — nothing can leave it.
		{"retired", "retiring", "retired cannot re-enter retiring"},
		{"retired", "ready", "retired cannot return to ready"},
		{"retired", "draining", "retired cannot drain"},
		{"retired", "drained", "retired cannot re-drain"},
		{"retired", "degraded", "retired cannot degrade"},
		{"retired", "unhealthy", "retired cannot become unhealthy"},
		// retiring can only move to retired.
		{"retiring", "ready", "retiring cannot return to ready"},
		{"retiring", "draining", "retiring cannot drain"},
		{"retiring", "drained", "retiring cannot re-drain"},
		{"retiring", "degraded", "retiring cannot degrade"},
		{"retiring", "unhealthy", "retiring cannot become unhealthy"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			err := ValidateHostTransition(tc.from, tc.to)
			if err == nil {
				t.Errorf("%s: expected ErrIllegalHostTransition for %s→%s, got nil",
					tc.desc, tc.from, tc.to)
			}
			if !errors.Is(err, ErrIllegalHostTransition) {
				t.Errorf("expected ErrIllegalHostTransition, got: %v", err)
			}
		})
	}
}

// ── MarkHostRetiring ──────────────────────────────────────────────────────────

func TestMarkHostRetiring_CASSucceeds_WhenNoActiveWorkload(t *testing.T) {
	// QueryRow returns count=0 (no active instances), Exec returns 1 row affected.
	pool := &fakePool{
		queryRowResult: fakeRow{values: []interface{}{0}}, // CountActiveInstancesOnHost
		execRows:       1,
	}
	r := newRepo(pool)

	activeCount, updated, err := r.MarkHostRetiring(ctx(), "host_001", 3, "drained", ReasonOperatorRetired)
	if err != nil {
		t.Fatalf("MarkHostRetiring: %v", err)
	}
	if activeCount != 0 {
		t.Errorf("activeCount = %d, want 0", activeCount)
	}
	if !updated {
		t.Error("expected updated=true when 1 row affected")
	}
	// Verify reason_code arg was passed correctly.
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	// args: hostID=$1, fromGeneration=$2, reasonVal=$3, fromStatus=$4
	if call.args[0] != "host_001" {
		t.Errorf("arg[0] = %v, want host_001", call.args[0])
	}
	if call.args[2] != ReasonOperatorRetired {
		t.Errorf("arg[2] = %v, want %q", call.args[2], ReasonOperatorRetired)
	}
	if call.args[3] != "drained" {
		t.Errorf("arg[3] = %v, want drained", call.args[3])
	}
}

func TestMarkHostRetiring_Blocked_WhenActiveWorkloadRemains(t *testing.T) {
	// QueryRow returns count=3 (active instances remain).
	pool := &fakePool{
		queryRowResult: fakeRow{values: []interface{}{3}},
	}
	r := newRepo(pool)

	activeCount, updated, err := r.MarkHostRetiring(ctx(), "host_001", 3, "drained", ReasonOperatorRetired)
	if err != nil {
		t.Fatalf("MarkHostRetiring: %v", err)
	}
	if activeCount != 3 {
		t.Errorf("activeCount = %d, want 3 (workload blocks retirement)", activeCount)
	}
	if updated {
		t.Error("expected updated=false when workload remains")
	}
	// No Exec should have been issued — blocked before CAS.
	if len(pool.execCalls) != 0 {
		t.Errorf("expected 0 Exec calls when blocked, got %d", len(pool.execCalls))
	}
}

func TestMarkHostRetiring_CASFails_WhenGenerationMismatch(t *testing.T) {
	// QueryRow returns count=0, Exec returns 0 rows (generation mismatch).
	pool := &fakePool{
		queryRowResult: fakeRow{values: []interface{}{0}},
		execRows:       0,
	}
	r := newRepo(pool)

	activeCount, updated, err := r.MarkHostRetiring(ctx(), "host_001", 99, "drained", ReasonOperatorRetired)
	if err != nil {
		t.Fatalf("MarkHostRetiring: %v", err)
	}
	if activeCount != 0 {
		t.Errorf("activeCount = %d, want 0", activeCount)
	}
	if updated {
		t.Error("expected updated=false when CAS fails")
	}
}

func TestMarkHostRetiring_IllegalTransition_ReturnsError(t *testing.T) {
	pool := &fakePool{}
	r := newRepo(pool)

	// ready → retiring is not in legalTransitions.
	_, _, err := r.MarkHostRetiring(ctx(), "host_001", 0, "ready", ReasonOperatorRetired)
	if err == nil {
		t.Error("expected ErrIllegalHostTransition for ready→retiring, got nil")
	}
	if !errors.Is(err, ErrIllegalHostTransition) {
		t.Errorf("expected ErrIllegalHostTransition, got: %v", err)
	}
	// No count query or Exec should have been issued before transition validation.
	if len(pool.execCalls) != 0 {
		t.Errorf("expected 0 Exec calls on illegal transition, got %d", len(pool.execCalls))
	}
}

func TestMarkHostRetiring_EmptyReasonCode_StoresNULL(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []interface{}{0}},
		execRows:       1,
	}
	r := newRepo(pool)

	_, _, err := r.MarkHostRetiring(ctx(), "host_001", 0, "drained", "")
	if err != nil {
		t.Fatalf("MarkHostRetiring: %v", err)
	}
	call := pool.execCalls[0]
	// arg[2] is reasonVal — must be nil when reasonCode is empty.
	if call.args[2] != nil {
		t.Errorf("expected reason_code arg to be nil for empty string, got %v", call.args[2])
	}
}

func TestMarkHostRetiring_PropagatesCountError(t *testing.T) {
	pool := &fakePool{
		queryRowErr: fmt.Errorf("db count error"),
	}
	r := newRepo(pool)

	_, _, err := r.MarkHostRetiring(ctx(), "host_001", 0, "drained", ReasonOperatorRetired)
	if err == nil {
		t.Error("expected error from count query, got nil")
	}
}

func TestMarkHostRetiring_PropagatesExecError(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []interface{}{0}},
		execErr:        fmt.Errorf("db exec error"),
	}
	r := newRepo(pool)

	_, _, err := r.MarkHostRetiring(ctx(), "host_001", 0, "drained", ReasonOperatorRetired)
	if err == nil {
		t.Error("expected error from Exec, got nil")
	}
}

// ── MarkHostRetired ───────────────────────────────────────────────────────────

func TestMarkHostRetired_CASSucceeds_SetsRetiredAt(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	updated, err := r.MarkHostRetired(ctx(), "host_001", 5)
	if err != nil {
		t.Fatalf("MarkHostRetired: %v", err)
	}
	if !updated {
		t.Error("expected updated=true when 1 row affected")
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	// args: hostID=$1, fromGeneration=$2
	if call.args[0] != "host_001" {
		t.Errorf("arg[0] = %v, want host_001", call.args[0])
	}
	if call.args[1] != int64(5) {
		t.Errorf("arg[1] = %v, want int64(5)", call.args[1])
	}
	// Verify retired_at = NOW() is expressed in the SQL (check via query text).
	if !containsSubstr(call.query, "retired_at") {
		t.Error("expected retired_at to appear in UPDATE query")
	}
	if !containsSubstr(call.query, "retired") {
		t.Error("expected status='retired' in UPDATE query")
	}
}

func TestMarkHostRetired_CASFails_WhenGenerationMismatch(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	updated, err := r.MarkHostRetired(ctx(), "host_001", 99)
	if err != nil {
		t.Fatalf("MarkHostRetired: %v", err)
	}
	if updated {
		t.Error("expected updated=false when CAS fails (0 rows affected)")
	}
}

func TestMarkHostRetired_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db error")}
	r := newRepo(pool)

	_, err := r.MarkHostRetired(ctx(), "host_001", 0)
	if err == nil {
		t.Error("expected error from Exec, got nil")
	}
}

// ── GetRetiredHosts ───────────────────────────────────────────────────────────

func TestGetRetiredHosts_ReturnsEmptySlice_WhenNoneExist(t *testing.T) {
	pool := &fakePool{queryRowsData: nil}
	r := newRepo(pool)

	hosts, err := r.GetRetiredHosts(ctx())
	if err != nil {
		t.Fatalf("GetRetiredHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("expected 0 hosts, got %d", len(hosts))
	}
}

// ── ReasonOperatorRetired constant ───────────────────────────────────────────

func TestReasonOperatorRetired_Defined(t *testing.T) {
	if ReasonOperatorRetired == "" {
		t.Error("ReasonOperatorRetired constant is empty string")
	}
	// Confirm it is not in the fence-required set
	// (retirement is deliberate; fence_required must not be set).
	if fenceRequiredReasons[ReasonOperatorRetired] {
		t.Error("ReasonOperatorRetired must not be in fenceRequiredReasons")
	}
}

// ── HostRecord.IsSchedulable with retiring/retired ────────────────────────────

func TestIsSchedulable_RetiringAndRetired_AreFalse(t *testing.T) {
	for _, status := range []string{"retiring", "retired"} {
		h := &HostRecord{Status: status}
		if h.IsSchedulable() {
			t.Errorf("status=%q: IsSchedulable()=true, want false", status)
		}
	}
}

// ── helper ────────────────────────────────────────────────────────────────────

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSubstrScan(s, sub))
}

func containsSubstrScan(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
