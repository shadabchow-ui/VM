package runtime

// firecracker_dryrun_test.go — Unit tests for FIRECRACKER_DRY_RUN mode.
//
// Verifies that when FIRECRACKER_DRY_RUN=true, StartVM:
//   - returns before any os.MkdirAll under /run or other runtime dirs
//   - does not exec the firecracker binary
//   - returns a stable synthetic PID (dryRunPID) and nil error
//   - is idempotent (repeated calls return same PID)
//
// No firecracker binary, no /run access, no root required.
// Run: go test ./services/host-agent/runtime/... -run TestFirecrackerDryRun -v

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

// newDryRunFirecrackerManager returns a FirecrackerManager with FIRECRACKER_DRY_RUN=true
// and deliberately unreachable runtime dirs to prove no writes are attempted.
func newDryRunFirecrackerManager(t *testing.T) *FirecrackerManager {
	t.Helper()
	t.Setenv("FIRECRACKER_DRY_RUN", "true")

	// Use paths that are guaranteed to be unwritable so any accidental
	// mkdir/write call would cause the test to fail rather than silently succeed.
	mgr := NewFirecrackerManager(
		"/run/firecracker-DRYRUN-SENTINEL",      // socketDir — must never be created
		"/run/firecracker-DRYRUN-SENTINEL/pids", // pidDir — must never be created
		"/nonexistent/vmlinux",                  // kernelPath — must never be opened
		slog.New(slog.NewTextHandler(os.Stderr, nil)),
	)
	if !mgr.dryRun {
		t.Fatal("FIRECRACKER_DRY_RUN=true but dryRun field is false")
	}
	return mgr
}

// minimalStartVMRequest returns the minimum valid StartVMRequest for dry-run tests.
func minimalStartVMRequest(instanceID string) *StartVMRequest {
	return &StartVMRequest{
		InstanceID: instanceID,
		RootfsPath: "/nonexistent/rootfs.qcow2",
		CPUCores:   1,
		MemoryMB:   512,
		TapDevice:  "tap-dryrun",
		MacAddress: "02:aa:bb:cc:dd:01",
		PrivateIP:  "10.0.0.1",
	}
}

// TestFirecrackerDryRun_StartVM_ReturnsSyntheticPID is the primary regression
// test for the read-only /run failure.
// Before the fix: StartVM called os.MkdirAll("/run/firecracker") and crashed.
// After the fix: StartVM returns (dryRunPID, nil) before any mkdir.
func TestFirecrackerDryRun_StartVM_ReturnsSyntheticPID(t *testing.T) {
	mgr := newDryRunFirecrackerManager(t)

	pid, err := mgr.StartVM(context.Background(), minimalStartVMRequest("inst_dryrun01"))
	if err != nil {
		t.Fatalf("StartVM dry-run: unexpected error: %v", err)
	}
	if pid != dryRunPID {
		t.Errorf("StartVM dry-run: got pid %d, want dryRunPID (%d)", pid, dryRunPID)
	}
}

// TestFirecrackerDryRun_StartVM_NoRuntimeDirCreated asserts that the sentinel
// directory was never created on the filesystem.
func TestFirecrackerDryRun_StartVM_NoRuntimeDirCreated(t *testing.T) {
	mgr := newDryRunFirecrackerManager(t)
	sentinelDir := "/run/firecracker-DRYRUN-SENTINEL"

	// Pre-condition: directory must not exist before the call.
	if _, err := os.Stat(sentinelDir); err == nil {
		t.Skipf("sentinel dir %s exists — cannot assert it was not created", sentinelDir)
	}

	_, _ = mgr.StartVM(context.Background(), minimalStartVMRequest("inst_dryrun02"))

	if _, err := os.Stat(sentinelDir); err == nil {
		t.Errorf("StartVM dry-run created sentinel dir %s — mkdirAll was not short-circuited", sentinelDir)
		_ = os.Remove(sentinelDir) // best-effort cleanup so other tests are not affected
	}
}

// TestFirecrackerDryRun_StartVM_Idempotent verifies that calling StartVM
// twice in dry-run mode returns the same PID both times.
func TestFirecrackerDryRun_StartVM_Idempotent(t *testing.T) {
	mgr := newDryRunFirecrackerManager(t)
	req := minimalStartVMRequest("inst_dryrun03")

	pid1, err := mgr.StartVM(context.Background(), req)
	if err != nil {
		t.Fatalf("first StartVM: %v", err)
	}
	pid2, err := mgr.StartVM(context.Background(), req)
	if err != nil {
		t.Fatalf("second StartVM: %v", err)
	}
	if pid1 != pid2 {
		t.Errorf("idempotency: first pid %d != second pid %d", pid1, pid2)
	}
}

// TestFirecrackerDryRun_StartVM_DifferentInstances verifies that two different
// instance IDs both get the same stable synthetic PID (since no real process
// tracking occurs in dry-run mode).
func TestFirecrackerDryRun_StartVM_DifferentInstances(t *testing.T) {
	mgr := newDryRunFirecrackerManager(t)

	pid1, err := mgr.StartVM(context.Background(), minimalStartVMRequest("inst_dryrunA"))
	if err != nil {
		t.Fatalf("StartVM A: %v", err)
	}
	pid2, err := mgr.StartVM(context.Background(), minimalStartVMRequest("inst_dryrunB"))
	if err != nil {
		t.Fatalf("StartVM B: %v", err)
	}
	if pid1 != dryRunPID || pid2 != dryRunPID {
		t.Errorf("expected both PIDs == dryRunPID (%d), got %d and %d", dryRunPID, pid1, pid2)
	}
}

// TestFirecrackerDryRun_ProductionManagerNotDryRun verifies that a manager
// constructed without FIRECRACKER_DRY_RUN=true has dryRun=false,
// i.e. the production code path is not accidentally short-circuited.
func TestFirecrackerDryRun_ProductionManagerNotDryRun(t *testing.T) {
	// Ensure the env var is absent for this test.
	t.Setenv("FIRECRACKER_DRY_RUN", "")

	mgr := NewFirecrackerManager("", "", "", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if mgr.dryRun {
		t.Error("production manager has dryRun=true with FIRECRACKER_DRY_RUN unset — production path would be skipped")
	}
}

// TestDryRunPID_OutsideLinuxPIDRange documents that dryRunPID is intentionally
// larger than the Linux kernel's maximum PID (4194304 on 64-bit kernels)
// so no real process could share this value.
func TestDryRunPID_OutsideLinuxPIDRange(t *testing.T) {
	const maxLinuxPID = 4194304
	if dryRunPID <= maxLinuxPID {
		t.Errorf("dryRunPID (%d) is within the Linux PID range (<= %d) — "+
			"a real process could have this PID; choose a larger sentinel value",
			dryRunPID, maxLinuxPID)
	}
}

// TestFirecracker_ExtraDisks_ReturnsError verifies that FirecrackerManager.Create
// returns a clear unsupported error when InstanceSpec has ExtraDisks.
// The Firecracker backend does not support additional block devices.
func TestFirecracker_ExtraDisks_ReturnsError(t *testing.T) {
	t.Setenv("FIRECRACKER_DRY_RUN", "true")
	mgr := newDryRunFirecrackerManager(t)

	spec := InstanceSpec{
		InstanceID: "inst-fc-extras",
		CPUCores:   2,
		MemoryMB:   4096,
		RootfsPath: "/tmp/test.qcow2",
		ExtraDisks: []ExtraDisk{
			{DiskID: "vol-data1", HostPath: "/path/to/disk.img", DeviceName: "/dev/vdb"},
		},
	}

	_, err := mgr.Create(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for extra disks on Firecracker, got nil")
	}
	if err.Error() == "" {
		t.Error("error message is empty")
	}
	t.Logf("Firecracker extra-disk error: %v", err)
}

// TestFirecracker_NoExtraDisks_Succeeds verifies that FirecrackerManager.Create
// succeeds when no ExtraDisks are present (existing behaviour preserved).
func TestFirecracker_NoExtraDisks_Succeeds(t *testing.T) {
	t.Setenv("FIRECRACKER_DRY_RUN", "true")
	mgr := newDryRunFirecrackerManager(t)

	spec := InstanceSpec{
		InstanceID: "inst-fc-noextras",
		CPUCores:   2,
		MemoryMB:   4096,
		RootfsPath: "/tmp/test.qcow2",
		TapDevice:  "tap-noextras",
		MacAddress: "02:00:00:00:00:FC",
	}

	info, err := mgr.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create without ExtraDisks: %v", err)
	}
	if info.State != "RUNNING" {
		t.Errorf("expected RUNNING state, got %q", info.State)
	}
}
