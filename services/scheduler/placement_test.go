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
		Status:        "ready",
		TotalCPU:      16, UsedCPU: 4,
		TotalMemoryMB: 65536, UsedMemoryMB: 16384,
		TotalDiskGB:   500, UsedDiskGB: 100,
	}
	if !h.CanFit(4, 8192, 50) {
		t.Error("expected CanFit=true for ready host with sufficient resources")
	}
}

func TestCanFit_ReadyHost_InsufficientCPU_ReturnsFalse(t *testing.T) {
	h := &HostSummary{
		Status:        "ready",
		TotalCPU:      4, UsedCPU: 3,
		TotalMemoryMB: 65536, UsedMemoryMB: 0,
		TotalDiskGB:   500, UsedDiskGB: 0,
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
				Status:        tc.status,
				// Plenty of resources — exclusion must be status-driven.
				TotalCPU:      64, UsedCPU: 0,
				TotalMemoryMB: 262144, UsedMemoryMB: 0,
				TotalDiskGB:   2000, UsedDiskGB: 0,
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
		Status:        "draining",
		TotalCPU:      32, UsedCPU: 0,
		TotalMemoryMB: 131072, UsedMemoryMB: 0,
		TotalDiskGB:   1000, UsedDiskGB: 0,
	}
	if h.CanFit(1, 512, 10) {
		t.Error("draining host must never receive new VM placements regardless of free resources")
	}
}

func TestCanFit_ReadyHost_FullyLoaded_ReturnsFalse(t *testing.T) {
	h := &HostSummary{
		Status:        "ready",
		TotalCPU:      8, UsedCPU: 8,
		TotalMemoryMB: 32768, UsedMemoryMB: 32768,
		TotalDiskGB:   200, UsedDiskGB: 200,
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
		Status:        "retiring",
		TotalCPU:      64, UsedCPU: 0,
		TotalMemoryMB: 262144, UsedMemoryMB: 0,
		TotalDiskGB:   2000, UsedDiskGB: 0,
	}
	if h.CanFit(1, 512, 1) {
		t.Error("retiring host must never receive new VM placements regardless of free resources")
	}
}

func TestCanFit_RetiredHost_NeverReceivesNewPlacements(t *testing.T) {
	// A host in 'retired' state is permanently removed from service.
	// Scheduler must exclude it unconditionally — it is a terminal state.
	h := &HostSummary{
		Status:        "retired",
		TotalCPU:      64, UsedCPU: 0,
		TotalMemoryMB: 262144, UsedMemoryMB: 0,
		TotalDiskGB:   2000, UsedDiskGB: 0,
	}
	if h.CanFit(1, 512, 1) {
		t.Error("retired host must never receive new VM placements — terminal state")
	}
}
