package reconciler

// classifier.go — Drift classifier: read-only analysis of one instance.
//
// ClassifyDrift is pure analysis — no DB writes, no runtime calls.
// It examines the DB instance row and returns a structured DriftResult.
// The reconciler passes this result to the Dispatcher, which creates repair jobs.
//
// Phase 1 drift classes (all five required by IMPLEMENTATION_PLAN_V1 §WS-3):
//
//   DriftNone                  — system is healthy, no action needed
//   DriftStuckProvisioning     — instance has been provisioning > 15 min
//   DriftWrongRuntimeState     — instance in unexpected transitional state too long
//   DriftMissingRuntimeProcess — instance assigned to host but no activity detected
//   DriftOrphanedResource      — instance in terminal state but resources not cleaned
//   DriftJobTimeout            — active job claim is stale (no claimed_at progress)
//
// Source: 03-03-reconciliation-loops-and-state-authority.md §Drift Classification,
//         IMPLEMENTATION_PLAN_V1 §WS-3 (outputs: 5 drift classes),
//         LIFECYCLE_STATE_MACHINE_V1 §5 (failure timeouts per state).

import (
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// DriftClass enumerates all detectable Phase 1 drift classes.
// Source: IMPLEMENTATION_PLAN_V1 §WS-3.
// VM Job 5 additions: host_unhealthy_with_live_instance, volume_orphan_artifact,
// network_stale_for_deleted, attachment_missing_runtime.
type DriftClass string

const (
	DriftNone                  DriftClass = "none"
	DriftStuckProvisioning     DriftClass = "stuck_provisioning"
	DriftWrongRuntimeState     DriftClass = "wrong_runtime_state"
	DriftMissingRuntimeProcess DriftClass = "missing_runtime_process"
	DriftOrphanedResource      DriftClass = "orphaned_resource"
	DriftJobTimeout            DriftClass = "job_timeout"

	// VM Job 5: reconciliation hardening drift classes.
	//
	// DriftHostUnhealthyWithLiveInstance — instance is running on a host that is
	// degraded/unhealthy with a stale heartbeat. The instance state may be stale
	// and the host may be fenced. No automatic repair; logged for operator action.
	DriftHostUnhealthyWithLiveInstance DriftClass = "host_unhealthy_with_live_instance"

	// DriftVolumeOrphanArtifact — a volume has a storage_path persisted but the
	// volume status is deleted. The storage artifact is orphaned and may be
	// safe to clean up. Conservative: only when volume is fully deleted.
	DriftVolumeOrphanArtifact DriftClass = "volume_orphan_artifact"

	// DriftNetworkStaleForDeleted — a network interface (NIC) exists with
	// IP allocation for an instance that is already deleted. The NIC + IP
	// are stale and safe to clean up.
	DriftNetworkStaleForDeleted DriftClass = "network_stale_for_deleted"

	// DriftAttachmentMissingRuntime — a volume attachment record exists in DB
	// (attachment active) but the volume or instance is in a state that makes
	// the attachment impossible (volume deleted, instance deleted).
	DriftAttachmentMissingRuntime DriftClass = "attachment_missing_runtime"

	// ── Runtime-aware drift classes ─────────────────────────────────────────
	//
	// These drift classes compare the DB instance state against actual runtime
	// inventory reported by the host agent. They are detect-only — the reconciler
	// writes events and does NOT perform destructive cleanup or auto-remediation.

	// DriftDBRunningNoRuntime — DB says running but host-agent ListInstances
	// reports no matching runtime process. The VM process may have crashed or
	// the host may have rebooted. Detect-only; emits event for operator awareness.
	DriftDBRunningNoRuntime DriftClass = "db_running_no_runtime"

	// DriftDBStoppedRuntimePresent — DB says stopped/deleted but host-agent
	// still reports a runtime process/artifact. The cleanup job may have failed
	// silently or the worker missed the delete. Detect-only; emits event.
	DriftDBStoppedRuntimePresent DriftClass = "db_stopped_runtime_present"

	// DriftOrphanRuntimeProcess — host-agent reports a runtime process for an
	// instance_id that is not known to the DB at all. The instance may have been
	// deleted from DB without runtime cleanup. Detect-only; emits event.
	DriftOrphanRuntimeProcess DriftClass = "orphan_runtime_process"

	// DriftStaleHostArtifacts — DB instance is deleted/terminal but the host
	// has disk paths, TAP devices, or socket paths still present. This is a
	// superset of db_stopped_runtime_present but scoped to terminal DB states.
	DriftStaleHostArtifacts DriftClass = "stale_host_artifacts"
)

// Collision avoidance: assign a unique idempotency scope key to each new class
// for use when generating dispatcher idempotency keys.
var driftClassScopeKeys = map[DriftClass]string{
	DriftHostUnhealthyWithLiveInstance: "hhl",
	DriftVolumeOrphanArtifact:          "voa",
	DriftNetworkStaleForDeleted:        "nsd",
	DriftAttachmentMissingRuntime:      "amr",
	DriftDBRunningNoRuntime:            "drn",
	DriftDBStoppedRuntimePresent:       "dsr",
	DriftOrphanRuntimeProcess:          "orp",
	DriftStaleHostArtifacts:            "sha",
}

// Phase 1 drift detection thresholds.
// Source: LIFECYCLE_STATE_MACHINE_V1 §5 (Failure timeouts per state).
const (
	// stuckProvisioningThreshold: 15 min, from LIFECYCLE_STATE_MACHINE_V1 §5.
	stuckProvisioningThreshold = 15 * time.Minute
	// stuckTransitionalThreshold: general transitional state watchdog.
	// Applies to stopping, rebooting, deleting.
	stuckTransitionalThreshold = 10 * time.Minute
	// missingRuntimeThreshold: instance assigned to host with no observed progress.
	// Source: 03-03 §Missing-Runtime-Process "no actual_state report for > 5 min".
	missingRuntimeThreshold = 5 * time.Minute
	// stuckRequestedThreshold: instance stuck in requested before worker claims.
	// Source: LIFECYCLE_STATE_MACHINE_V1 §5 "Stuck in REQUESTED: 5 minutes".
	stuckRequestedThreshold = 5 * time.Minute
)

// DriftResult is the output of ClassifyDrift. The reconciler acts on this.
type DriftResult struct {
	// Class is the detected drift category.
	Class DriftClass
	// RepairJobType is the job type to dispatch for repair.
	// Empty when Class == DriftNone or when repair requires only state update.
	RepairJobType string
	// Reason is a human-readable explanation for logging.
	Reason string
}

// NoDrift is the zero-value result for healthy instances.
var NoDrift = DriftResult{Class: DriftNone}

// ClassifyDrift analyses a single instance row and returns its drift class.
// now is injected so tests can control the clock.
// This function performs no I/O — it is pure analysis.
// Source: 03-03-reconciliation-loops §Drift Classification and Repair Actions.
func ClassifyDrift(inst *db.InstanceRow, now time.Time) DriftResult {
	age := now.Sub(inst.UpdatedAt)

	switch inst.VMState {

	// ── REQUESTED ─────────────────────────────────────────────────────────────
	// Should transition to PROVISIONING almost immediately (worker claims the
	// job and advances it). If stuck for > 5 min, the job is likely lost.
	case "requested":
		if age > stuckRequestedThreshold {
			return DriftResult{
				Class:         DriftStuckProvisioning,
				RepairJobType: "",
				Reason:        "instance stuck in requested state for > 5 min — job may be lost",
			}
		}

	// ── PROVISIONING ──────────────────────────────────────────────────────────
	// Must complete within 15 minutes. Longer indicates a stuck worker or
	// host-agent failure.
	// Source: LIFECYCLE_STATE_MACHINE_V1 §5 "Stuck in PROVISIONING: 15 minutes".
	case "provisioning":
		if age > stuckProvisioningThreshold {
			return DriftResult{
				Class:         DriftStuckProvisioning,
				RepairJobType: "",
				Reason:        "instance stuck in provisioning for > 15 min",
			}
		}

	// ── STOPPING ──────────────────────────────────────────────────────────────
	// Stop must complete within 10 minutes. If still stopping after that,
	// the stop job is stuck and needs re-dispatch.
	// Source: LIFECYCLE_STATE_MACHINE_V1 §5 "Stop failure — hard phase: 10 min".
	case "stopping":
		if age > stuckTransitionalThreshold {
			return DriftResult{
				Class:         DriftWrongRuntimeState,
				RepairJobType: "INSTANCE_STOP",
				Reason:        "instance stuck in stopping state for > 10 min",
			}
		}

	// ── REBOOTING ─────────────────────────────────────────────────────────────
	// Reboot must complete within 3 minutes.
	// Source: LIFECYCLE_STATE_MACHINE_V1 §5 "Stuck in REBOOTING: 3 minutes".
	case "rebooting":
		rebootThreshold := 3 * time.Minute
		if age > rebootThreshold {
			return DriftResult{
				Class:         DriftWrongRuntimeState,
				RepairJobType: "INSTANCE_REBOOT",
				Reason:        "instance stuck in rebooting state for > 3 min",
			}
		}

	// ── DELETING ──────────────────────────────────────────────────────────────
	// Delete must complete within its job's max_attempts * timeout window.
	// Flag after general transitional threshold.
	case "deleting":
		if age > stuckTransitionalThreshold {
			return DriftResult{
				Class:         DriftWrongRuntimeState,
				RepairJobType: "INSTANCE_DELETE",
				Reason:        "instance stuck in deleting state for > 10 min",
			}
		}

	// ── RUNNING ───────────────────────────────────────────────────────────────
	// Healthy stable state. Check for missing runtime process signal:
	// instance assigned to a host but UpdatedAt is very stale suggests
	// the host stopped reporting.
	// Source: 03-03 §Missing-Runtime-Process.
	case "running":
		if inst.HostID == nil {
			// Running but not assigned to any host — orphaned resource.
			// Source: 03-03 §Orphaned-Resource.
			return DriftResult{
				Class:         DriftOrphanedResource,
				RepairJobType: "",
				Reason:        "instance in running state with no host assigned",
			}
		}
		// If UpdatedAt is very stale, the host-agent may have stopped reporting.
		// Phase 1: flag as MissingRuntimeProcess for operator awareness.
		// We do not auto-reboot — that is Phase 2 auto-recovery.
		// Source: 03-03 §Missing-Runtime-Process "Do not reschedule. Automatic
		// placement is a Phase 2 feature."
		if age > missingRuntimeThreshold {
			return DriftResult{
				Class:         DriftMissingRuntimeProcess,
				RepairJobType: "",
				Reason:        "instance running but no state update from host for > 5 min",
			}
		}

	// ── STOPPED ───────────────────────────────────────────────────────────────
	// Healthy stable state. No automatic repair in Phase 1.
	case "stopped":
		// No drift classification for stable stopped instances.

	// ── FAILED, DELETED ───────────────────────────────────────────────────────
	// Terminal states. No reconciliation action.
	case "failed", "deleted":
		// Nothing to do.
	}

	return NoDrift
}

// ── VM Job 5: extended classification helpers ─────────────────────────────────

// ClassifyHostInstanceDrift detects drift for an instance whose host health is
// known. Called only when the reconciler has host data available (e.g. during
// periodic resync when host records are also scanned).
//
// When the instance is running on a host that is degraded, unhealthy, or has a
// stale heartbeat, surface DriftHostUnhealthyWithLiveInstance. This does NOT
// auto-repair — the host may be fenced. The event is observable for operators.
func ClassifyHostInstanceDrift(inst *db.InstanceRow, hostStatus string, heartbeatAge time.Duration, now time.Time) DriftResult {
	if inst.VMState != "running" {
		return NoDrift
	}
	if hostStatus == "" {
		return NoDrift
	}

	// Host is unhealthy or degraded → the running instance may be stale.
	if hostStatus == "unhealthy" || hostStatus == "degraded" {
		return DriftResult{
			Class:  DriftHostUnhealthyWithLiveInstance,
			Reason: "instance running on host with status " + hostStatus,
		}
	}

	// Host is ready but heartbeat is very stale (> 5 min).
	if heartbeatAge > 5*time.Minute {
		return DriftResult{
			Class:  DriftHostUnhealthyWithLiveInstance,
			Reason: "instance running on host with stale heartbeat",
		}
	}

	return NoDrift
}

// ClassifyVolumeOrphanArtifact detects volumes that have a storage_path set
// but are in deleted status. The storage artifact is orphaned.
// Pure analysis — never mutates.
func ClassifyVolumeOrphanArtifact(volStatus string, storagePath *string) DriftResult {
	if volStatus == "deleted" && storagePath != nil && *storagePath != "" {
		return DriftResult{
			Class:  DriftVolumeOrphanArtifact,
			Reason: "volume deleted but storage_path still references artifact",
		}
	}
	return NoDrift
}

// ClassifyNetworkStaleForDeleted detects NIC rows whose instance has been deleted.
// Pure analysis — the caller passes instanceDeleted (bool from cross-reference).
func ClassifyNetworkStaleForDeleted(instanceDeleted bool, nicStatus string) DriftResult {
	if instanceDeleted && nicStatus != "deleted" {
		return DriftResult{
			Class:  DriftNetworkStaleForDeleted,
			Reason: "network interface for deleted instance is not cleaned up",
		}
	}
	return NoDrift
}

// ClassifyAttachmentMissingRuntime detects volume attachments where the owning
// instance or volume is in a terminal state that makes the attachment impossible.
func ClassifyAttachmentMissingRuntime(attachmentActive bool, instanceState string, volumeState string) DriftResult {
	if !attachmentActive {
		return NoDrift
	}
	if instanceState == "deleted" || instanceState == "failed" {
		return DriftResult{
			Class:  DriftAttachmentMissingRuntime,
			Reason: "volume attachment exists but instance is " + instanceState,
		}
	}
	if volumeState == "deleted" || volumeState == "error" {
		return DriftResult{
			Class:  DriftAttachmentMissingRuntime,
			Reason: "volume attachment exists but volume is " + volumeState,
		}
	}
	return NoDrift
}

// ── Runtime-aware drift classification ────────────────────────────────────────

// RuntimeInventoryEntry is a lightweight view of one runtime process/artefact
// as reported by the host agent's ListInstances.
type RuntimeInventoryEntry struct {
	InstanceID string
	State      string // "RUNNING", "STOPPED", "DELETED"
	PID        int32
	HasTap     bool
	HasDisk    bool
}

// ClassifyRuntimeDrift compares the DB instance against runtime inventory to
// detect four runtime-aware drift classes.
//
// runtimeByID maps instanceID → RuntimeInventoryEntry for the host this instance
// is assigned to. Pass nil or empty map when the host is unreachable (no runtime
// truth available) — in that case only DB-side analysis is performed.
//
// dbDeleted should be true when the DB instance row has been soft-deleted or
// the vm_state is "deleted" or "failed".
//
// All detected drifts are detect-only — no destructive mutations.
func ClassifyRuntimeDrift(inst *db.InstanceRow, runtimeByID map[string]RuntimeInventoryEntry) DriftResult {
	runtime, hasRuntime := runtimeByID[inst.ID]

	switch {
	case inst.VMState == "running" && inst.HostID != nil:
		if !hasRuntime || !runtime.IsPresent() {
			// DB says running+assigned but runtime inventory says nothing.
			return DriftResult{
				Class:  DriftDBRunningNoRuntime,
				Reason: "DB says running but runtime reports no matching process or artifact",
			}
		}

	case inst.VMState == "stopped" || inst.VMState == "deleting":
		if hasRuntime && runtime.IsPresent() {
			return DriftResult{
				Class:  DriftDBStoppedRuntimePresent,
				Reason: "DB says " + inst.VMState + " but runtime still reports process/artifact for instance " + inst.ID,
			}
		}

	case inst.VMState == "deleted" || inst.VMState == "failed":
		if hasRuntime && runtime.IsPresent() {
			return DriftResult{
				Class:  DriftStaleHostArtifacts,
				Reason: "DB instance is " + inst.VMState + " but host has residual runtime artifacts",
			}
		}
	}

	return NoDrift
}

// ClassifyOrphanRuntimes identifies runtime processes that exist on a host but
// have no corresponding DB instance record (the instance was deleted from DB
// without cleanup, or the runtime process belongs to an unknown instance).
//
// Returns a slice of DriftResults — one per orphan. The DriftResult.Reason
// contains the instance_id from the runtime inventory.
func ClassifyOrphanRuntimes(runtimeByID map[string]RuntimeInventoryEntry, dbInstanceIDs map[string]bool) []DriftResult {
	var orphans []DriftResult
	for id, entry := range runtimeByID {
		if !dbInstanceIDs[id] && entry.IsPresent() {
			orphans = append(orphans, DriftResult{
				Class:  DriftOrphanRuntimeProcess,
				Reason: "runtime process " + id + " has no corresponding DB instance",
			})
		}
	}
	return orphans
}

// IsPresent returns true when the runtime entry represents a live process or artefact.
func (e *RuntimeInventoryEntry) IsPresent() bool {
	return e.State != "" && e.State != "DELETED"
}
