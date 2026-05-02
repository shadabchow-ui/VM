package runtime

// rediscovery.go — Host-agent startup rediscovery of expected running VMs.
//
// VM Job 5 — Case 8: Host-agent restart rediscovers expected running VM state.
//
// On startup, the host-agent scans the pid directory for *.pid files and
// determines which VMs were running before the agent restarted. This enables
// the agent to report accurate runtime state even after a crash or planned
// restart.
//
// The ListInstances RPC already serves this purpose (scanning PID files on
// demand). This file adds a startup-time rediscovery check that:
//   1. Scans PID files.
//   2. Checks process liveness.
//   3. Logs a summary of discovered VMs.
//   4. Returns the list so the registration path can include it.
//
// This does NOT modify DB state — it is purely an observability seam.
// The control-plane reconciler is responsible for cross-referencing this
// against DB desired state.
//
// Source: RUNTIMESERVICE_GRPC_V1 §7 (ListInstances),
//         IMPLEMENTATION_PLAN_V1 §29 (host-agent crash recovery).

import (
	"log/slog"
)

// RediscoveryResult captures the host-agent's startup runtime scan.
type RediscoveryResult struct {
	RunningVMs    []RediscoveredVM
	StalePIDFiles []string // PID file exists but process is dead
}

// RediscoveredVM describes a VM that was found running at agent startup.
type RediscoveredVM struct {
	InstanceID string
	PID        int
	State      string // "RUNNING" or "STOPPED"
}

// RediscoverInstances scans the pid directory for VMs that were running
// before the host-agent (re)started. This is the startup rediscovery seam
// for crash recovery.
func (f *FirecrackerManager) RediscoverInstances(log *slog.Logger) (*RediscoveryResult, error) {
	entries, err := f.listPIDFiles()
	if err != nil {
		return nil, err
	}

	result := &RediscoveryResult{}

	for _, instanceID := range entries {
		pid, readErr := f.readPID(instanceID)
		if readErr != nil || pid == 0 {
			result.StalePIDFiles = append(result.StalePIDFiles, instanceID)
			continue
		}

		vm := RediscoveredVM{
			InstanceID: instanceID,
			PID:        pid,
			State:      "STOPPED",
		}
		if f.processAlive(pid) {
			vm.State = "RUNNING"
		} else {
			result.StalePIDFiles = append(result.StalePIDFiles, instanceID)
		}

		result.RunningVMs = append(result.RunningVMs, vm)
	}

	log.Info("host-agent startup rediscovery complete",
		"total_pid_files", len(entries),
		"running_vms", len(result.RunningVMs)-len(result.StalePIDFiles),
		"stale_pid_files", len(result.StalePIDFiles),
	)

	return result, nil
}

// CleanupStalePID removes PID files for processes that are no longer alive.
// Called after rediscovery to prevent stale state from accumulating.
func (f *FirecrackerManager) CleanupStalePID(instanceIDs []string) {
	for _, id := range instanceIDs {
		_ = f.cleanup(id)
	}
}
