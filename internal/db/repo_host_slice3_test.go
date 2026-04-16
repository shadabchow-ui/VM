package db

// repo_host_slice3_test.go — Unit tests for VM-P2E Slice 3 host lifecycle DB methods.
//
// Tests cover:
//   - ValidateHostTransition: legal and illegal transition pairs
//   - MarkHostDegraded: CAS success, CAS failure, illegal transition, empty reason
//   - MarkHostUnhealthy: CAS success with fence_required=TRUE/FALSE, CAS failure,
//     illegal transition
//   - ClearFenceRequired: success, CAS failure (wrong generation), no-op when
//     fence_required already false
//   - GetFenceRequiredHosts: uses Query path
//   - HostRecord.IsSchedulable: only ready returns true
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"errors"
	"fmt"
	"testing"
)

// ── ValidateHostTransition ─────────────────────────────────────────────────────

func TestValidateHostTransition_LegalPairs(t *testing.T) {
	cases := []struct {
		from, to string
	}{
		{"ready", "draining"},
		{"ready", "degraded"},
		{"ready", "unhealthy"},
		{"draining", "drained"},
		{"draining", "degraded"},
		{"draining", "unhealthy"},
		{"drained", "ready"},
		{"drained", "degraded"},
		{"degraded", "ready"},
		{"degraded", "unhealthy"},
		{"degraded", "draining"},
		{"unhealthy", "degraded"},
		{"unhealthy", "ready"},
		{"", "ready"}, // new host
	}
	for _, tc := range cases {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			if err := ValidateHostTransition(tc.from, tc.to); err != nil {
				t.Errorf("expected no error for %s→%s, got: %v", tc.from, tc.to, err)
			}
		})
	}
}

func TestValidateHostTransition_IllegalPairs(t *testing.T) {
	cases := []struct {
		from, to string
	}{
		{"drained", "unhealthy"},    // drained cannot go directly to unhealthy
		{"drained", "draining"},     // drained cannot go back to draining
		{"unhealthy", "draining"},   // unhealthy cannot drain
		{"unhealthy", "drained"},    // unhealthy cannot drain-complete
		{"degraded", "drained"},     // degraded cannot skip to drained
		{"ready", "drained"},        // ready cannot skip drain steps
		{"draining", "ready"},       // draining cannot go back to ready directly
		{"fenced", "ready"},         // fenced is not in legalTransitions yet (Slice 4)
	}
	for _, tc := range cases {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			err := ValidateHostTransition(tc.from, tc.to)
			if err == nil {
				t.Errorf("expected error for illegal transition %s→%s, got nil", tc.from, tc.to)
			}
			if !errors.Is(err, ErrIllegalHostTransition) {
				t.Errorf("expected ErrIllegalHostTransition, got: %v", err)
			}
		})
	}
}

func TestValidateHostTransition_UnknownFromStatus(t *testing.T) {
	err := ValidateHostTransition("bogus", "ready")
	if err == nil {
		t.Error("expected error for unknown fromStatus, got nil")
	}
	if !errors.Is(err, ErrIllegalHostTransition) {
		t.Errorf("expected ErrIllegalHostTransition, got: %v", err)
	}
}

// ── MarkHostDegraded ──────────────────────────────────────────────────────────

func TestMarkHostDegraded_CASSucceeds(t *testing.T) {
	pool := &fakePool{execRows: 1} // 1 row affected → CAS succeeded
	r := newRepo(pool)

	ok, err := r.MarkHostDegraded(ctx(), "host_001", 2, "ready", ReasonAgentUnresponsive)
	if err != nil {
		t.Fatalf("MarkHostDegraded: %v", err)
	}
	if !ok {
		t.Error("expected updated=true when 1 row affected")
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	// args: hostID=$1, fromGeneration=$2, reasonVal=$3, fromStatus=$4
	if call.args[0] != "host_001" {
		t.Errorf("arg[0] = %v, want host_001", call.args[0])
	}
	if call.args[1] != int64(2) {
		t.Errorf("arg[1] = %v, want int64(2)", call.args[1])
	}
}

func TestMarkHostDegraded_CASFails_WhenGenerationMismatch(t *testing.T) {
	pool := &fakePool{execRows: 0} // 0 rows → CAS failed
	r := newRepo(pool)

	ok, err := r.MarkHostDegraded(ctx(), "host_001", 99, "ready", ReasonAgentUnresponsive)
	if err != nil {
		t.Fatalf("MarkHostDegraded: %v", err)
	}
	if ok {
		t.Error("expected updated=false when 0 rows affected")
	}
}

func TestMarkHostDegraded_IllegalTransition_ReturnsError(t *testing.T) {
	pool := &fakePool{}
	r := newRepo(pool)

	// drained → degraded is legal; drained → unhealthy directly is not in
	// the degraded path. Use a clearly illegal pair:
	// unhealthy → draining is not in legalTransitions for unhealthy→degraded path.
	// Actually let's use: drained → draining (illegal).
	_, err := r.MarkHostDegraded(ctx(), "host_001", 0, "unhealthy", ReasonAgentFailed)
	// unhealthy → degraded IS legal, so let's use a truly illegal pair:
	// "draining" → "degraded" is legal too. Use "drained" → "draining" which
	// is illegal, but the target here is "degraded" not "draining".
	// Let's use fromStatus="fenced" which is not in legalTransitions at all.
	if err == nil {
		// unhealthy → degraded is legal so err should be nil above
		t.Log("unhealthy → degraded is legal, as expected")
	}
	// Re-test with a truly illegal pair: fromStatus not in legalTransitions
	pool2 := &fakePool{}
	r2 := newRepo(pool2)
	_, err = r2.MarkHostDegraded(ctx(), "host_001", 0, "fenced", ReasonAgentFailed)
	if err == nil {
		t.Error("expected ErrIllegalHostTransition for fenced→degraded, got nil")
	}
	if !errors.Is(err, ErrIllegalHostTransition) {
		t.Errorf("expected ErrIllegalHostTransition, got: %v", err)
	}
	// No Exec should have been issued since validation fails before DB call.
	if len(pool2.execCalls) != 0 {
		t.Errorf("expected 0 Exec calls on illegal transition, got %d", len(pool2.execCalls))
	}
}

func TestMarkHostDegraded_EmptyReasonCode_StoresNULL(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	_, err := r.MarkHostDegraded(ctx(), "host_001", 0, "ready", "")
	if err != nil {
		t.Fatalf("MarkHostDegraded: %v", err)
	}
	call := pool.execCalls[0]
	// arg[2] is reasonVal — should be nil when reasonCode is empty
	if call.args[2] != nil {
		t.Errorf("expected reason_code arg to be nil for empty string, got %v", call.args[2])
	}
}

func TestMarkHostDegraded_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db down")}
	r := newRepo(pool)

	_, err := r.MarkHostDegraded(ctx(), "host_001", 0, "ready", ReasonAgentFailed)
	if err == nil {
		t.Error("expected error from Exec, got nil")
	}
}

// ── MarkHostUnhealthy ─────────────────────────────────────────────────────────

func TestMarkHostUnhealthy_AmbiguousReason_SetsFenceRequired(t *testing.T) {
	ambiguousReasons := []string{
		ReasonAgentUnresponsive,
		ReasonHypervisorFailed,
		ReasonNetworkUnreachable,
	}
	for _, reason := range ambiguousReasons {
		reason := reason
		t.Run(reason, func(t *testing.T) {
			pool := &fakePool{execRows: 1}
			r := newRepo(pool)

			fenceReq, ok, err := r.MarkHostUnhealthy(ctx(), "host_001", 0, "degraded", reason)
			if err != nil {
				t.Fatalf("MarkHostUnhealthy: %v", err)
			}
			if !ok {
				t.Error("expected updated=true")
			}
			if !fenceReq {
				t.Errorf("reason=%q: expected fenceRequired=true for ambiguous failure", reason)
			}
			// Verify fence_required=TRUE was passed to the UPDATE
			call := pool.execCalls[0]
			// args: hostID=$1, fromGeneration=$2, reasonVal=$3, fenceRequired=$4, fromStatus=$5
			if call.args[3] != true {
				t.Errorf("expected fence_required arg=true, got %v", call.args[3])
			}
		})
	}
}

func TestMarkHostUnhealthy_NonAmbiguousReason_DoesNotSetFenceRequired(t *testing.T) {
	nonAmbiguousReasons := []string{
		ReasonAgentFailed,
		ReasonStorageError,
		ReasonOperatorDegraded,
		ReasonOperatorUnhealthy,
	}
	for _, reason := range nonAmbiguousReasons {
		reason := reason
		t.Run(reason, func(t *testing.T) {
			pool := &fakePool{execRows: 1}
			r := newRepo(pool)

			fenceReq, ok, err := r.MarkHostUnhealthy(ctx(), "host_001", 0, "degraded", reason)
			if err != nil {
				t.Fatalf("MarkHostUnhealthy: %v", err)
			}
			if !ok {
				t.Error("expected updated=true")
			}
			if fenceReq {
				t.Errorf("reason=%q: expected fenceRequired=false for non-ambiguous failure", reason)
			}
			call := pool.execCalls[0]
			if call.args[3] != false {
				t.Errorf("expected fence_required arg=false, got %v", call.args[3])
			}
		})
	}
}

func TestMarkHostUnhealthy_CASFails(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	fenceReq, ok, err := r.MarkHostUnhealthy(ctx(), "host_001", 99, "degraded", ReasonAgentUnresponsive)
	if err != nil {
		t.Fatalf("MarkHostUnhealthy: %v", err)
	}
	if ok {
		t.Error("expected updated=false when CAS fails")
	}
	if fenceReq {
		t.Error("expected fenceRequired=false when CAS fails")
	}
}

func TestMarkHostUnhealthy_IllegalTransition_ReturnsError(t *testing.T) {
	pool := &fakePool{}
	r := newRepo(pool)

	// drained → unhealthy is illegal (not in legalTransitions["drained"])
	_, _, err := r.MarkHostUnhealthy(ctx(), "host_001", 0, "drained", ReasonAgentFailed)
	if err == nil {
		t.Error("expected ErrIllegalHostTransition for drained→unhealthy, got nil")
	}
	if !errors.Is(err, ErrIllegalHostTransition) {
		t.Errorf("expected ErrIllegalHostTransition, got: %v", err)
	}
	if len(pool.execCalls) != 0 {
		t.Errorf("expected 0 Exec calls on illegal transition, got %d", len(pool.execCalls))
	}
}

func TestMarkHostUnhealthy_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("connection reset")}
	r := newRepo(pool)

	_, _, err := r.MarkHostUnhealthy(ctx(), "host_001", 0, "degraded", ReasonAgentFailed)
	if err == nil {
		t.Error("expected error from Exec, got nil")
	}
}

// ── ClearFenceRequired ────────────────────────────────────────────────────────

func TestClearFenceRequired_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	ok, err := r.ClearFenceRequired(ctx(), "host_001", 3)
	if err != nil {
		t.Fatalf("ClearFenceRequired: %v", err)
	}
	if !ok {
		t.Error("expected updated=true when 1 row affected")
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "host_001" {
		t.Errorf("arg[0] = %v, want host_001", call.args[0])
	}
	if call.args[1] != int64(3) {
		t.Errorf("arg[1] = %v, want int64(3)", call.args[1])
	}
}

func TestClearFenceRequired_CASFails_WhenAlreadyCleared(t *testing.T) {
	// fence_required = FALSE already → WHERE fence_required = TRUE does not match
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	ok, err := r.ClearFenceRequired(ctx(), "host_001", 3)
	if err != nil {
		t.Fatalf("ClearFenceRequired: %v", err)
	}
	if ok {
		t.Error("expected updated=false when no row matched (already cleared or wrong gen)")
	}
}

func TestClearFenceRequired_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db error")}
	r := newRepo(pool)

	_, err := r.ClearFenceRequired(ctx(), "host_001", 0)
	if err == nil {
		t.Error("expected error from Exec, got nil")
	}
}

// ── GetFenceRequiredHosts ─────────────────────────────────────────────────────

func TestGetFenceRequiredHosts_ReturnsEmptySlice_WhenNoneExist(t *testing.T) {
	// Query returns no rows
	pool := &fakePool{queryRowsData: nil}
	r := newRepo(pool)

	hosts, err := r.GetFenceRequiredHosts(ctx())
	if err != nil {
		t.Fatalf("GetFenceRequiredHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("expected 0 hosts, got %d", len(hosts))
	}
}

// ── HostRecord.IsSchedulable ──────────────────────────────────────────────────

func TestIsSchedulable_OnlyReadyIsTrue(t *testing.T) {
	cases := []struct {
		status       string
		schedulable  bool
	}{
		{"ready", true},
		{"draining", false},
		{"drained", false},
		{"degraded", false},
		{"unhealthy", false},
		{"fenced", false},
		{"retired", false},
		{"offline", false},
		{"maintenance", false},
		{"provisioning", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			h := &HostRecord{Status: tc.status}
			if h.IsSchedulable() != tc.schedulable {
				t.Errorf("status=%q: IsSchedulable()=%v, want %v", tc.status, h.IsSchedulable(), tc.schedulable)
			}
		})
	}
}

// ── ReasonCode constants ──────────────────────────────────────────────────────

func TestReasonCodeConstants_AllDefined(t *testing.T) {
	codes := []string{
		ReasonAgentUnresponsive,
		ReasonAgentFailed,
		ReasonStorageError,
		ReasonHypervisorFailed,
		ReasonNetworkUnreachable,
		ReasonOperatorDegraded,
		ReasonOperatorUnhealthy,
	}
	for _, c := range codes {
		if c == "" {
			t.Error("reason code constant is empty string")
		}
	}
}

func TestFenceRequiredReasons_AmbiguousSet(t *testing.T) {
	// Verify the fence-required set is exactly the three ambiguous reasons.
	expectedFence := map[string]bool{
		ReasonAgentUnresponsive:  true,
		ReasonHypervisorFailed:   true,
		ReasonNetworkUnreachable: true,
	}
	expectedNoFence := []string{
		ReasonAgentFailed,
		ReasonStorageError,
		ReasonOperatorDegraded,
		ReasonOperatorUnhealthy,
	}
	for reason, expected := range expectedFence {
		if fenceRequiredReasons[reason] != expected {
			t.Errorf("fenceRequiredReasons[%q] = %v, want %v", reason, fenceRequiredReasons[reason], expected)
		}
	}
	for _, reason := range expectedNoFence {
		if fenceRequiredReasons[reason] {
			t.Errorf("fenceRequiredReasons[%q] = true, want false", reason)
		}
	}
}
