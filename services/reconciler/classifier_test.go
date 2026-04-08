package reconciler

// classifier_test.go — Unit tests for the drift classifier.
//
// ClassifyDrift is pure analysis — no I/O. Tests are fully deterministic.
// The clock is injected via the `now` parameter to keep tests reproducible.
//
// Coverage required by M4 gate:
//   - All 5 drift classes correctly detected
//   - No-drift case for healthy instances
//   - Threshold boundary: just under → NoDrift, just over → drift detected
//
// Source: 03-03-reconciliation-loops §Drift Classification,
//         IMPLEMENTATION_PLAN_V1 §WS-3 (5 drift classes).

import (
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func instWithState(state string, updatedAgo time.Duration) *db.InstanceRow {
	hostID := "host-001"
	now := time.Now()
	return &db.InstanceRow{
		ID:        "inst-classify-001",
		VMState:   state,
		HostID:    &hostID,
		Version:   1,
		UpdatedAt: now.Add(-updatedAgo),
	}
}

func instNoHost(state string, updatedAgo time.Duration) *db.InstanceRow {
	now := time.Now()
	return &db.InstanceRow{
		ID:        "inst-nohost-001",
		VMState:   state,
		HostID:    nil,
		Version:   1,
		UpdatedAt: now.Add(-updatedAgo),
	}
}

func classifyNow(inst *db.InstanceRow) DriftResult {
	return ClassifyDrift(inst, time.Now())
}

// ── DriftNone — healthy states ─────────────────────────────────────────────────

func TestClassifier_NoDrift_RunningRecentUpdate(t *testing.T) {
	inst := instWithState("running", 30*time.Second)
	result := classifyNow(inst)
	if result.Class != DriftNone {
		t.Errorf("recent running instance: class = %q, want none", result.Class)
	}
}

func TestClassifier_NoDrift_StoppedInstance(t *testing.T) {
	inst := instWithState("stopped", 30*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftNone {
		t.Errorf("stopped instance: class = %q, want none", result.Class)
	}
}

func TestClassifier_NoDrift_ProvisioningWithinThreshold(t *testing.T) {
	// 10 min < 15 min threshold → no drift yet
	inst := instWithState("provisioning", 10*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftNone {
		t.Errorf("provisioning within threshold: class = %q, want none", result.Class)
	}
}

func TestClassifier_NoDrift_RebootingWithinThreshold(t *testing.T) {
	// 1 min < 3 min threshold → no drift
	inst := instWithState("rebooting", 1*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftNone {
		t.Errorf("rebooting within threshold: class = %q, want none", result.Class)
	}
}

// ── DriftStuckProvisioning ─────────────────────────────────────────────────────

func TestClassifier_StuckProvisioning_ProvisioningOver15Min(t *testing.T) {
	// Source: LIFECYCLE_STATE_MACHINE_V1 §5 "Stuck in PROVISIONING: 15 minutes".
	inst := instWithState("provisioning", 16*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftStuckProvisioning {
		t.Errorf("provisioning 16min: class = %q, want stuck_provisioning", result.Class)
	}
}

func TestClassifier_StuckProvisioning_RequestedOver5Min(t *testing.T) {
	// Source: LIFECYCLE_STATE_MACHINE_V1 §5 "Stuck in REQUESTED: 5 minutes".
	inst := instWithState("requested", 6*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftStuckProvisioning {
		t.Errorf("requested 6min: class = %q, want stuck_provisioning", result.Class)
	}
}

func TestClassifier_NoStuckProvisioning_JustUnderThreshold(t *testing.T) {
	inst := instWithState("provisioning", 14*time.Minute+50*time.Second)
	result := classifyNow(inst)
	if result.Class != DriftNone {
		t.Errorf("provisioning just under threshold: class = %q, want none", result.Class)
	}
}

// ── DriftWrongRuntimeState ────────────────────────────────────────────────────

func TestClassifier_WrongRuntimeState_StoppingOver10Min(t *testing.T) {
	inst := instWithState("stopping", 11*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftWrongRuntimeState {
		t.Errorf("stopping 11min: class = %q, want wrong_runtime_state", result.Class)
	}
	if result.RepairJobType != "INSTANCE_STOP" {
		t.Errorf("RepairJobType = %q, want INSTANCE_STOP", result.RepairJobType)
	}
}

func TestClassifier_WrongRuntimeState_RebootingOver3Min(t *testing.T) {
	// Source: LIFECYCLE_STATE_MACHINE_V1 §5 "Stuck in REBOOTING: 3 minutes".
	inst := instWithState("rebooting", 4*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftWrongRuntimeState {
		t.Errorf("rebooting 4min: class = %q, want wrong_runtime_state", result.Class)
	}
	if result.RepairJobType != "INSTANCE_REBOOT" {
		t.Errorf("RepairJobType = %q, want INSTANCE_REBOOT", result.RepairJobType)
	}
}

func TestClassifier_WrongRuntimeState_DeletingOver10Min(t *testing.T) {
	inst := instWithState("deleting", 11*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftWrongRuntimeState {
		t.Errorf("deleting 11min: class = %q, want wrong_runtime_state", result.Class)
	}
	if result.RepairJobType != "INSTANCE_DELETE" {
		t.Errorf("RepairJobType = %q, want INSTANCE_DELETE", result.RepairJobType)
	}
}

// ── DriftMissingRuntimeProcess ────────────────────────────────────────────────

func TestClassifier_MissingRuntimeProcess_RunningNoUpdateFor5Min(t *testing.T) {
	// Source: 03-03 §Missing-Runtime-Process "no actual_state report for > 5 min".
	inst := instWithState("running", 6*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftMissingRuntimeProcess {
		t.Errorf("running 6min stale: class = %q, want missing_runtime_process", result.Class)
	}
}

func TestClassifier_NoMissingRuntime_RunningWithinWindow(t *testing.T) {
	inst := instWithState("running", 2*time.Minute)
	result := classifyNow(inst)
	if result.Class != DriftNone {
		t.Errorf("running 2min: class = %q, want none", result.Class)
	}
}

// ── DriftOrphanedResource ─────────────────────────────────────────────────────

func TestClassifier_OrphanedResource_RunningWithNoHost(t *testing.T) {
	// Source: 03-03 §Orphaned-Resource.
	inst := instNoHost("running", 10*time.Second)
	result := classifyNow(inst)
	if result.Class != DriftOrphanedResource {
		t.Errorf("running no host: class = %q, want orphaned_resource", result.Class)
	}
}

// ── Terminal states — always NoDrift ─────────────────────────────────────────

func TestClassifier_TerminalStates_NoDrift(t *testing.T) {
	for _, state := range []string{"failed", "deleted"} {
		t.Run("state="+state, func(t *testing.T) {
			inst := instWithState(state, 24*time.Hour)
			result := classifyNow(inst)
			if result.Class != DriftNone {
				t.Errorf("terminal state %q: class = %q, want none", state, result.Class)
			}
		})
	}
}

// ── Reason field populated on all non-none results ────────────────────────────

func TestClassifier_NonNone_HasReason(t *testing.T) {
	cases := []*db.InstanceRow{
		instWithState("provisioning", 20*time.Minute),
		instWithState("stopping", 15*time.Minute),
		instWithState("running", 10*time.Minute),
		instNoHost("running", 1*time.Second),
	}
	for _, inst := range cases {
		result := classifyNow(inst)
		if result.Class != DriftNone && result.Reason == "" {
			t.Errorf("state %q: drift class %q has empty reason", inst.VMState, result.Class)
		}
	}
}
