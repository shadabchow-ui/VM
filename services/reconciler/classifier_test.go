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

// ── VM Job 5: New drift classification tests ──────────────────────────────────

// TestClassifier_HostUnhealthyWithLiveInstance verifies that a running instance
// on a degraded or unhealthy host is flagged.
// VM Job 5 — Case 2: DB says running but host unhealthy.
func TestClassifier_HostUnhealthyWithLiveInstance_Degraded(t *testing.T) {
	inst := instWithState("running", 1*time.Minute)
	result := ClassifyHostInstanceDrift(inst, "degraded", 2*time.Minute, time.Now())
	if result.Class != DriftHostUnhealthyWithLiveInstance {
		t.Errorf("running on degraded host: class = %q, want host_unhealthy_with_live_instance", result.Class)
	}
}

func TestClassifier_HostUnhealthyWithLiveInstance_HealthyHost(t *testing.T) {
	inst := instWithState("running", 1*time.Minute)
	result := ClassifyHostInstanceDrift(inst, "ready", 1*time.Minute, time.Now())
	if result.Class != DriftNone {
		t.Errorf("running on healthy host: class = %q, want none", result.Class)
	}
}

func TestClassifier_HostUnhealthyWithLiveInstance_StaleHeartbeat(t *testing.T) {
	inst := instWithState("running", 1*time.Minute)
	result := ClassifyHostInstanceDrift(inst, "ready", 10*time.Minute, time.Now())
	if result.Class != DriftHostUnhealthyWithLiveInstance {
		t.Errorf("running on host with stale heartbeat: class = %q, want host_unhealthy_with_live_instance", result.Class)
	}
}

func TestClassifier_HostUnhealthyWithLiveInstance_NotRunning(t *testing.T) {
	inst := instWithState("stopped", 1*time.Minute)
	result := ClassifyHostInstanceDrift(inst, "degraded", 2*time.Minute, time.Now())
	if result.Class != DriftNone {
		t.Errorf("stopped instance on bad host: class = %q, want none (only running instances flagged)", result.Class)
	}
}

// TestClassifier_VolumeOrphanArtifact verifies detection of orphan storage.
// VM Job 5 — Case 6: Volume artifact exists but DB says deleted.
func TestClassifier_VolumeOrphanArtifact_Deleted(t *testing.T) {
	sp := "/mnt/vols/vol-abc.qcow2"
	result := ClassifyVolumeOrphanArtifact("deleted", &sp)
	if result.Class != DriftVolumeOrphanArtifact {
		t.Errorf("deleted volume with storage_path: class = %q, want volume_orphan_artifact", result.Class)
	}
}

func TestClassifier_VolumeOrphanArtifact_Available(t *testing.T) {
	sp := "/mnt/vols/vol-abc.qcow2"
	result := ClassifyVolumeOrphanArtifact("available", &sp)
	if result.Class != DriftNone {
		t.Errorf("available volume: class = %q, want none", result.Class)
	}
}

func TestClassifier_VolumeOrphanArtifact_NoPath(t *testing.T) {
	result := ClassifyVolumeOrphanArtifact("deleted", nil)
	if result.Class != DriftNone {
		t.Errorf("deleted volume with nil storage_path: class = %q, want none", result.Class)
	}
}

// TestClassifier_NetworkStaleForDeleted verifies stale NIC detection.
// VM Job 5 — Case 5: Stale TAP/NAT/firewall state for deleted/stopped instances.
func TestClassifier_NetworkStaleForDeleted(t *testing.T) {
	result := ClassifyNetworkStaleForDeleted(true, "attached")
	if result.Class != DriftNetworkStaleForDeleted {
		t.Errorf("stale NIC: class = %q, want network_stale_for_deleted", result.Class)
	}
}

func TestClassifier_NetworkStaleForDeleted_Cleaned(t *testing.T) {
	result := ClassifyNetworkStaleForDeleted(true, "deleted")
	if result.Class != DriftNone {
		t.Errorf("already-cleaned NIC: class = %q, want none", result.Class)
	}
}

func TestClassifier_NetworkStaleForDeleted_LiveInstance(t *testing.T) {
	result := ClassifyNetworkStaleForDeleted(false, "attached")
	if result.Class != DriftNone {
		t.Errorf("live instance NIC: class = %q, want none", result.Class)
	}
}

// TestClassifier_AttachmentMissingRuntime verifies stale attachment detection.
// VM Job 5 — Case 7: DB attachment intent exists but runtime disk attachment missing.
func TestClassifier_AttachmentMissingRuntime_InstanceDeleted(t *testing.T) {
	result := ClassifyAttachmentMissingRuntime(true, "deleted", "available")
	if result.Class != DriftAttachmentMissingRuntime {
		t.Errorf("attachment on deleted instance: class = %q, want attachment_missing_runtime", result.Class)
	}
}

func TestClassifier_AttachmentMissingRuntime_VolumeDeleted(t *testing.T) {
	result := ClassifyAttachmentMissingRuntime(true, "running", "deleted")
	if result.Class != DriftAttachmentMissingRuntime {
		t.Errorf("attachment on deleted volume: class = %q, want attachment_missing_runtime", result.Class)
	}
}

func TestClassifier_AttachmentMissingRuntime_Healthy(t *testing.T) {
	result := ClassifyAttachmentMissingRuntime(true, "running", "in_use")
	if result.Class != DriftNone {
		t.Errorf("healthy attachment: class = %q, want none", result.Class)
	}
}

func TestClassifier_AttachmentMissingRuntime_Inactive(t *testing.T) {
	result := ClassifyAttachmentMissingRuntime(false, "deleted", "deleted")
	if result.Class != DriftNone {
		t.Errorf("inactive attachment: class = %q, want none", result.Class)
	}
}

// ── Runtime-aware drift classifier tests ─────────────────────────────────────

func makeRuntimeEntry(id, state string, pid int32) RuntimeInventoryEntry {
	return RuntimeInventoryEntry{
		InstanceID: id,
		State:      state,
		PID:        pid,
		HasTap:     state == "RUNNING",
		HasDisk:    state != "DELETED",
	}
}

func TestClassifier_RuntimeDrift_DBRunningNoRuntime(t *testing.T) {
	inst := instWithState("running", 30*time.Second)
	// Empty runtime inventory — no runtime process found for this instance.
	runtimeByID := map[string]RuntimeInventoryEntry{}
	result := ClassifyRuntimeDrift(inst, runtimeByID)
	if result.Class != DriftDBRunningNoRuntime {
		t.Errorf("running instance with no runtime: class = %q, want db_running_no_runtime", result.Class)
	}
}

func TestClassifier_RuntimeDrift_DBRunningWithRuntime_Happy(t *testing.T) {
	inst := instWithState("running", 30*time.Second)
	runtimeByID := map[string]RuntimeInventoryEntry{
		inst.ID: makeRuntimeEntry(inst.ID, "RUNNING", 12345),
	}
	result := ClassifyRuntimeDrift(inst, runtimeByID)
	if result.Class != DriftNone {
		t.Errorf("running instance with matching runtime: class = %q, want none", result.Class)
	}
}

func TestClassifier_RuntimeDrift_DBStoppedRuntimePresent(t *testing.T) {
	inst := instWithState("stopped", 1*time.Minute)
	runtimeByID := map[string]RuntimeInventoryEntry{
		inst.ID: makeRuntimeEntry(inst.ID, "RUNNING", 12345),
	}
	result := ClassifyRuntimeDrift(inst, runtimeByID)
	if result.Class != DriftDBStoppedRuntimePresent {
		t.Errorf("stopped instance with runtime present: class = %q, want db_stopped_runtime_present", result.Class)
	}
}

func TestClassifier_RuntimeDrift_DBDeletingRuntimePresent(t *testing.T) {
	inst := instWithState("deleting", 2*time.Minute)
	runtimeByID := map[string]RuntimeInventoryEntry{
		inst.ID: makeRuntimeEntry(inst.ID, "RUNNING", 12345),
	}
	result := ClassifyRuntimeDrift(inst, runtimeByID)
	if result.Class != DriftDBStoppedRuntimePresent {
		t.Errorf("deleting instance with runtime present: class = %q, want db_stopped_runtime_present", result.Class)
	}
}

func TestClassifier_RuntimeDrift_DBDeletedStaleArtifacts(t *testing.T) {
	inst := instWithState("deleted", 1*time.Hour)
	runtimeByID := map[string]RuntimeInventoryEntry{
		inst.ID: makeRuntimeEntry(inst.ID, "STOPPED", 0),
	}
	result := ClassifyRuntimeDrift(inst, runtimeByID)
	if result.Class != DriftStaleHostArtifacts {
		t.Errorf("deleted instance with runtime artifacts: class = %q, want stale_host_artifacts", result.Class)
	}
}

func TestClassifier_RuntimeDrift_DBFailedStaleArtifacts(t *testing.T) {
	inst := instWithState("failed", 2*time.Hour)
	runtimeByID := map[string]RuntimeInventoryEntry{
		inst.ID: makeRuntimeEntry(inst.ID, "STOPPED", 0),
	}
	result := ClassifyRuntimeDrift(inst, runtimeByID)
	if result.Class != DriftStaleHostArtifacts {
		t.Errorf("failed instance with runtime artifacts: class = %q, want stale_host_artifacts", result.Class)
	}
}

func TestClassifier_RuntimeDrift_DBStoppedNoRuntime(t *testing.T) {
	inst := instWithState("stopped", 10*time.Minute)
	runtimeByID := map[string]RuntimeInventoryEntry{}
	result := ClassifyRuntimeDrift(inst, runtimeByID)
	if result.Class != DriftNone {
		t.Errorf("stopped instance with no runtime: class = %q, want none", result.Class)
	}
}

func TestClassifier_RuntimeDrift_NoRuntimeData_NilMap(t *testing.T) {
	inst := instWithState("running", 30*time.Second)
	result := ClassifyRuntimeDrift(inst, nil)
	if result.Class != DriftDBRunningNoRuntime {
		t.Errorf("running with nil runtime map: class = %q, want db_running_no_runtime", result.Class)
	}
}

func TestClassifier_OrphanRuntimes_SingleOrphan(t *testing.T) {
	runtimeByID := map[string]RuntimeInventoryEntry{
		"inst-orphan-001": makeRuntimeEntry("inst-orphan-001", "RUNNING", 1001),
		"inst-known-001":  makeRuntimeEntry("inst-known-001", "RUNNING", 1002),
	}
	dbIDs := map[string]bool{"inst-known-001": true}
	orphans := ClassifyOrphanRuntimes(runtimeByID, dbIDs)
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(orphans))
	}
	if orphans[0].Class != DriftOrphanRuntimeProcess {
		t.Errorf("orphan class = %q, want orphan_runtime_process", orphans[0].Class)
	}
}

func TestClassifier_OrphanRuntimes_NoOrphans(t *testing.T) {
	runtimeByID := map[string]RuntimeInventoryEntry{
		"inst-known-001": makeRuntimeEntry("inst-known-001", "RUNNING", 1001),
	}
	dbIDs := map[string]bool{"inst-known-001": true}
	orphans := ClassifyOrphanRuntimes(runtimeByID, dbIDs)
	if len(orphans) != 0 {
		t.Errorf("expected 0 orphans, got %d", len(orphans))
	}
}

func TestClassifier_OrphanRuntimes_EmptyHost(t *testing.T) {
	orphans := ClassifyOrphanRuntimes(map[string]RuntimeInventoryEntry{}, map[string]bool{})
	if len(orphans) != 0 {
		t.Errorf("expected 0 orphans on empty host, got %d", len(orphans))
	}
}

func TestClassifier_RuntimeDrift_DBStoppedRuntimeDelayed(t *testing.T) {
	inst := instWithState("stopped", 1*time.Hour)
	// Runtime reports DELETED — no artifact, clean.
	runtimeByID := map[string]RuntimeInventoryEntry{
		inst.ID: {InstanceID: inst.ID, State: "DELETED", PID: 0},
	}
	result := ClassifyRuntimeDrift(inst, runtimeByID)
	if result.Class != DriftNone {
		t.Errorf("stopped instance with deleted runtime: class = %q, want none", result.Class)
	}
}
