package main

// placement_test.go — Unit tests for Scheduler.CanFit drain-exclusion behavior.
//
// VM-P2E Slice 1: verifies that draining/drained/degraded/unhealthy/fenced/
// retired/offline/maintenance hosts are excluded from placement.
//
// Tests do NOT require a real Resource Manager or network — they test CanFit
// directly against a fabricated HostSummary.
//
// Source: vm-13-03__blueprint__ §core_contracts "Host State Atomicity" (drain
//         must be immediately visible to scheduler after status transition).

import "testing"

func TestCanFit_ReadyHost_WithSufficientResources_ReturnsTrue(t *testing.T) {
	h := &HostSummary{
		Status:   "ready",
		TotalCPU: 16, UsedCPU: 4,
		TotalMemoryMB: 65536, UsedMemoryMB: 16384,
		TotalDiskGB: 500, UsedDiskGB: 100,
	}
	if !h.CanFit(4, 8192, 50) {
		t.Error("expected CanFit=true for ready host with sufficient resources")
	}
}

func TestCanFit_ReadyHost_InsufficientCPU_ReturnsFalse(t *testing.T) {
	h := &HostSummary{
		Status:   "ready",
		TotalCPU: 4, UsedCPU: 3,
		TotalMemoryMB: 65536, UsedMemoryMB: 0,
		TotalDiskGB: 500, UsedDiskGB: 0,
	}
	if h.CanFit(4, 1024, 10) {
		t.Error("expected CanFit=false: only 1 free CPU, need 4")
	}
}

// ── VM-P2E Slice 1: drain status exclusion ────────────────────────────────────

// All non-ready statuses must prevent placement, regardless of available resources.
func TestCanFit_ExcludesNonReadyStatuses(t *testing.T) {
	cases := []struct {
		status string
	}{
		{"draining"},
		{"drained"},
		{"degraded"},
		{"unhealthy"},
		{"fenced"},
		{"retired"},
		{"offline"},
		{"maintenance"},
		{"provisioning"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.status, func(t *testing.T) {
			h := &HostSummary{
				Status: tc.status,
				// Plenty of resources — exclusion must be status-driven.
				TotalCPU: 64, UsedCPU: 0,
				TotalMemoryMB: 262144, UsedMemoryMB: 0,
				TotalDiskGB: 2000, UsedDiskGB: 0,
			}
			if h.CanFit(1, 512, 1) {
				t.Errorf("status=%q: expected CanFit=false (non-ready host must never receive new placements)", tc.status)
			}
		})
	}
}

func TestCanFit_DrainingHost_NeverReceivesNewPlacements(t *testing.T) {
	// This is the core P2E Slice 1 contract: marking a host draining is the
	// only admission gate. No secondary check is needed.
	h := &HostSummary{
		Status:   "draining",
		TotalCPU: 32, UsedCPU: 0,
		TotalMemoryMB: 131072, UsedMemoryMB: 0,
		TotalDiskGB: 1000, UsedDiskGB: 0,
	}
	if h.CanFit(1, 512, 10) {
		t.Error("draining host must never receive new VM placements regardless of free resources")
	}
}

func TestCanFit_ReadyHost_FullyLoaded_ReturnsFalse(t *testing.T) {
	h := &HostSummary{
		Status:   "ready",
		TotalCPU: 8, UsedCPU: 8,
		TotalMemoryMB: 32768, UsedMemoryMB: 32768,
		TotalDiskGB: 200, UsedDiskGB: 200,
	}
	if h.CanFit(1, 1024, 10) {
		t.Error("expected CanFit=false: host is fully loaded")
	}
}

// ── VM-P2E Slice 4: Retirement scheduler contract ─────────────────────────────
//
// These tests make the scheduler contract for retiring/retired hosts explicit.
// The table-driven test above already covers both statuses through the generic
// non-ready case; these named tests document the Slice 4 contract by name so
// any future change to CanFit that accidentally re-admits retiring or retired
// hosts produces a clearly named failure.

func TestCanFit_RetiringHost_NeverReceivesNewPlacements(t *testing.T) {
	// A host in 'retiring' state has been marked for permanent removal.
	// It must never receive new VM placements, regardless of available resources.
	h := &HostSummary{
		Status:   "retiring",
		TotalCPU: 64, UsedCPU: 0,
		TotalMemoryMB: 262144, UsedMemoryMB: 0,
		TotalDiskGB: 2000, UsedDiskGB: 0,
	}
	if h.CanFit(1, 512, 1) {
		t.Error("retiring host must never receive new VM placements regardless of free resources")
	}
}

func TestCanFit_RetiredHost_NeverReceivesNewPlacements(t *testing.T) {
	// A host in 'retired' state is permanently removed from service.
	// Scheduler must exclude it unconditionally — it is a terminal state.
	h := &HostSummary{
		Status:   "retired",
		TotalCPU: 64, UsedCPU: 0,
		TotalMemoryMB: 262144, UsedMemoryMB: 0,
		TotalDiskGB: 2000, UsedDiskGB: 0,
	}
	if h.CanFit(1, 512, 1) {
		t.Error("retired host must never receive new VM placements — terminal state")
	}
}

// ── VM Job 9: FenceRequired defense-in-depth ───────────────────────────────────

func TestCanFit_FenceRequiredTrue_StatusReady_Excluded(t *testing.T) {
	// Defense-in-depth: even if a host somehow has status=ready AND
	// fence_required=true (shouldn't happen via normal paths but possible
	// via a race), the scheduler must still reject placement.
	h := &HostSummary{
		ID:            "host-fr-001",
		Status:        "ready",
		FenceRequired: true,
		TotalCPU:      32, UsedCPU: 0,
		TotalMemoryMB: 65536, UsedMemoryMB: 0,
		TotalDiskGB: 500, UsedDiskGB: 0,
	}
	if h.CanFit(1, 512, 1) {
		t.Error("fence_required=TRUE host must never receive placements regardless of status")
	}
}

func TestCanFit_FenceRequiredFalse_StatusReady_Allowed(t *testing.T) {
	// Normal healthy host — should pass.
	h := &HostSummary{
		ID:            "host-fr-002",
		Status:        "ready",
		FenceRequired: false,
		TotalCPU:      32, UsedCPU: 0,
		TotalMemoryMB: 65536, UsedMemoryMB: 0,
		TotalDiskGB: 500, UsedDiskGB: 0,
	}
	if !h.CanFit(1, 512, 1) {
		t.Error("healthy ready host with fence_required=FALSE should allow placements")
	}
}

func TestCanFit_FenceRequiredHost_StatusUnhealthy_Excluded(t *testing.T) {
	// An unhealthy host with fence_required=TRUE must be excluded on both
	// status (not ready) and fence_required gates.
	h := &HostSummary{
		ID:            "host-fr-003",
		Status:        "unhealthy",
		FenceRequired: true,
		TotalCPU:      64, UsedCPU: 0,
		TotalMemoryMB: 131072, UsedMemoryMB: 0,
		TotalDiskGB: 1000, UsedDiskGB: 0,
	}
	if h.CanFit(1, 512, 1) {
		t.Error("unhealthy+fence_required host must never receive new placements")
	}
}

// ── VM Job 9: Scheduler reason code observability ─────────────────────────────

func TestCanFit_DegradedHost_WithReasonCode_Excluded(t *testing.T) {
	// A degraded host with an explicit reason code must be excluded from placement.
	// The reason_code is operator-visible via GET /status, not via scheduler list.
	h := &HostSummary{
		Status:   "degraded",
		TotalCPU: 16, UsedCPU: 0,
		TotalMemoryMB: 32768, UsedMemoryMB: 0,
		TotalDiskGB: 200, UsedDiskGB: 0,
	}
	if h.CanFit(1, 1024, 1) {
		t.Error("degraded host must never receive new placements")
	}
}

func TestCanFit_UnhealthyHost_FenceRequiredExcluded(t *testing.T) {
	// Reset fence_required on an already-unhealthy host to verify the
	// status gate is independent of the fence_required gate.
	h := &HostSummary{
		Status:        "unhealthy",
		FenceRequired: false,
		TotalCPU:      16, UsedCPU: 0,
		TotalMemoryMB: 32768, UsedMemoryMB: 0,
		TotalDiskGB: 200, UsedDiskGB: 0,
	}
	if h.CanFit(1, 1024, 1) {
		t.Error("unhealthy host (even without fence_required) must never receive placements")
	}
}

// ── VM Job 9: Stale heartbeat contract for scheduler ──────────────────────────
//
// These tests document the exit criterion for the scheduler: when the Resource
// Manager's GetAvailableHosts returns hosts, the scheduler trusts that stale
// hosts have already been excluded. There is no secondary stale check in the
// scheduler — the DB query is the single point of enforcement.

func TestCanFit_RecentlyHeartbeatingReadyHost_Allowed(t *testing.T) {
	// The GetAvailableHosts query returns this host because last_heartbeat_at
	// is within the 90-second window. Scheduler must accept it.
	h := &HostSummary{
		ID:            "host-hb-001",
		Status:        "ready",
		FenceRequired: false,
		TotalCPU:      8, UsedCPU: 2,
		TotalMemoryMB: 16384, UsedMemoryMB: 4096,
		TotalDiskGB: 100, UsedDiskGB: 10,
	}
	if !h.CanFit(4, 8192, 50) {
		t.Error("recently-heartbeating ready host must be eligible for placement")
	}
}
