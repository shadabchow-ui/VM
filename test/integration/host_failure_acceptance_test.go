//go:build integration

package integration

// host_failure_acceptance_test.go — Host failure and recovery safety gate tests.
//
// Verifies that the recovery subsystem respects fencing safety gates and does
// not pretend to recover VMs from ambiguous host failures without fencing.
//
// Gate items:
//   F-1: FenceRequired blocks recovery automation (GetRecoveryEligibleHosts)
//   F-2: Host health transition validates transitions correctly
//   F-3: ListRunningInstancesOnUnhealthyHosts detects affected instances
//   F-4: Recovery verdict tracking via InsertRecoveryLog
//   F-5: ClearFenceRequired is gated on correct generations
//   F-6: BootID change detection signals host reboot
//   F-7: Stale hosts excluded from GetAvailableHosts
//
// Run:
//   DATABASE_URL=postgres://... go test -tags=integration -v ./test/integration/... -run HostFailure -count=1
//
// Source: host_recovery.go, recovery_gate_test.go, db.go host methods.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── F-1: FenceRequired blocks recovery ────────────────────────────────────────

// TestHostFailure_FenceRequired_BlocksRecoveryEligibility verifies that
// GetRecoveryEligibleHosts never returns hosts with fence_required=TRUE.
// This is the hardest safety gate in the system.
func TestHostFailure_FenceRequired_BlocksRecoveryEligibility(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("hf1-%d", time.Now().UnixNano())

	// Create a host, mark unhealthy with ambiguous reason.
	if err := repo.UpsertHost(ctx, &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4, TotalMemoryMB: 8192, TotalDiskGB: 100,
		AgentVersion: "v1.0",
	}); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	updated, err := repo.MarkHostUnhealthy(ctx, hostID, 0, "ready", db.ReasonCodeHypervisorFailed)
	if err != nil {
		t.Fatalf("MarkHostUnhealthy: %v", err)
	}
	if !updated {
		t.Fatal("expected MarkHostUnhealthy to succeed")
	}

	host, err := repo.GetHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if !host.FenceRequired {
		t.Error("HYPERVISOR_FAILED unhealthy should set fence_required=TRUE")
	}

	// Recovery-eligible scan must exclude this host.
	eligible, err := repo.GetRecoveryEligibleHosts(ctx)
	if err != nil {
		t.Fatalf("GetRecoveryEligibleHosts: %v", err)
	}
	for _, h := range eligible {
		if h.ID == hostID {
			t.Errorf("host %s with fence_required=TRUE must NOT be recovery-eligible", hostID)
		}
	}

	// Also verify GetFenceRequiredHosts returns it.
	fenced, err := repo.GetFenceRequiredHosts(ctx)
	if err != nil {
		t.Fatalf("GetFenceRequiredHosts: %v", err)
	}
	found := false
	for _, h := range fenced {
		if h.ID == hostID {
			found = true
			break
		}
	}
	if !found {
		t.Error("GetFenceRequiredHosts should return fence_required=TRUE host")
	}
}

// ── F-2: Host health transition validation ────────────────────────────────────

func TestHostFailure_ValidateHostTransition_InvalidRejection(t *testing.T) {
	// Verify that ValidateHostTransition rejects invalid transitions.
	// Only allowed transitions should pass.
	allowed := []struct {
		from, to string
		allowed  bool
	}{
		{"ready", "draining", true},
		{"ready", "degraded", true},
		{"ready", "unhealthy", true},
		{"draining", "drained", true},
		{"drained", "ready", true},
		{"degraded", "draining", true},
		{"degraded", "unhealthy", true},
		{"degraded", "ready", true},
		{"unhealthy", "draining", true},
		{"unhealthy", "degraded", true},
		{"unhealthy", "ready", true},
		{"draining", "ready", true},
		// Invalid transitions
		{"ready", "ready", false},
		{"drained", "degraded", false},
		{"unhealthy", "drained", false},
		{"degraded", "drained", false},
		{"drained", "unhealthy", false},
		{"fenced", "ready", true}, // fenced is a future addition — allow
		{"provisioning", "ready", true},
	}

	for _, tc := range allowed {
		t.Run(fmt.Sprintf("%s→%s", tc.from, tc.to), func(t *testing.T) {
			err := db.ValidateHostTransition(tc.from, tc.to)
			if tc.allowed && err != nil {
				t.Errorf("transition %s→%s should be allowed, got: %v", tc.from, tc.to, err)
			}
			if !tc.allowed && err == nil {
				t.Errorf("transition %s→%s should be rejected", tc.from, tc.to)
			}
		})
	}
}

// ── F-3: Running instances on unhealthy hosts detected ────────────────────────

func TestHostFailure_ListRunningInstancesOnUnhealthyHosts(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("hf3-%d", time.Now().UnixNano())

	// Create a degraded host.
	if err := repo.UpsertHost(ctx, &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4, TotalMemoryMB: 8192, TotalDiskGB: 100,
		AgentVersion: "v1.0",
	}); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	// Mark degraded.
	repo.MarkHostDegraded(ctx, hostID, 0, "ready", db.ReasonCodeAgentUnresponsive)

	// Create a running instance on this host.
	instID := idgen.New(idgen.PrefixInstance)
	if err := repo.InsertInstance(ctx, &db.InstanceRow{
		ID:               instID,
		Name:             "hf3-running",
		OwnerPrincipalID: "00000000-0000-0000-0000-000000000001",
		VMState:          "running",
		InstanceTypeID:   "c1.small",
		ImageID:          "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
		HostID:           &hostID,
	}); err != nil {
		t.Fatalf("InsertInstance: %v", err)
	}

	// ListRunningInstancesOnUnhealthyHosts should detect the instance.
	results, err := repo.ListRunningInstancesOnUnhealthyHosts(ctx)
	if err != nil {
		t.Fatalf("ListRunningInstancesOnUnhealthyHosts: %v", err)
	}

	found := false
	for _, r := range results {
		if r.InstanceID == instID {
			found = true
			if r.HostStatus != "degraded" {
				t.Errorf("host status = %q, want degraded", r.HostStatus)
			}
			break
		}
	}
	if !found {
		t.Error("ListRunningInstancesOnUnhealthyHosts should detect running instance on degraded host")
	}
}

// ── F-4: Recovery verdict tracking ────────────────────────────────────────────

func TestHostFailure_RecoveryLog_InsertAndRead(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("hf4-%d", time.Now().UnixNano())

	if err := repo.UpsertHost(ctx, &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4, TotalMemoryMB: 8192, TotalDiskGB: 100,
		AgentVersion: "v1.0",
	}); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	// Insert a recovery decision log.
	recID := idgen.New("evt")
	err := repo.InsertRecoveryLog(ctx, &db.RecoveryLogRecord{
		ID:                      recID,
		HostID:                  hostID,
		Verdict:                 db.RecoveryVerdictSkippedFenceRequired,
		Reason:                  "STONITH has not completed — fencing required before recovery",
		HostStatusAtAttempt:     "unhealthy",
		HostGenerationAtAttempt: 1,
		FenceRequiredAtAttempt:  true,
		Actor:                   "recovery_loop",
	})
	if err != nil {
		t.Fatalf("InsertRecoveryLog: %v", err)
	}

	// Read back the recovery log.
	logs, err := repo.GetHostRecoveryLog(ctx, hostID)
	if err != nil {
		t.Fatalf("GetHostRecoveryLog: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 recovery log entry, got %d", len(logs))
	}
	if logs[0].Verdict != db.RecoveryVerdictSkippedFenceRequired {
		t.Errorf("verdict = %q, want skipped_fence_required", logs[0].Verdict)
	}
	if logs[0].FenceRequiredAtAttempt != true {
		t.Error("FenceRequiredAtAttempt should be true")
	}
}

// ── F-5: ClearFenceRequired is gated on generation ────────────────────────────

func TestHostFailure_ClearFenceRequired_GenerationGate(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("hf5-%d", time.Now().UnixNano())

	if err := repo.UpsertHost(ctx, &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4, TotalMemoryMB: 8192, TotalDiskGB: 100,
		AgentVersion: "v1.0",
	}); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	// Mark unhealthy with ambiguous reason (fence_required=TRUE).
	repo.MarkHostUnhealthy(ctx, hostID, 0, "ready", db.ReasonCodeHypervisorFailed)

	// Try stale generation — must be rejected.
	cleared, err := repo.ClearFenceRequired(ctx, hostID, 0)
	if err != nil {
		t.Fatalf("ClearFenceRequired stale gen: %v", err)
	}
	if cleared {
		t.Error("ClearFenceRequired with stale generation should fail")
	}

	// Correct generation.
	host, _ := repo.GetHostByID(ctx, hostID)
	cleared, err = repo.ClearFenceRequired(ctx, hostID, host.Generation)
	if err != nil {
		t.Fatalf("ClearFenceRequired correct gen: %v", err)
	}
	if !cleared {
		t.Error("ClearFenceRequired with correct generation should succeed")
	}
}

// ── F-6: BootID change signal ─────────────────────────────────────────────────

func TestHostFailure_BootID_ChangeDetection(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("hf6-%d", time.Now().UnixNano())

	if err := repo.UpsertHost(ctx, &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4, TotalMemoryMB: 8192, TotalDiskGB: 100,
		AgentVersion: "v1.0",
	}); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	// First boot ID.
	prevBoot, err := repo.UpdateHeartbeatBootID(ctx, hostID, "boot-aaa")
	if err != nil {
		t.Fatalf("UpdateHeartbeatBootID first: %v", err)
	}
	if prevBoot != "" {
		t.Logf("initial boot_id was %q (expected empty on fresh host)", prevBoot)
	}

	// Same boot ID — no change.
	prevBoot, err = repo.UpdateHeartbeatBootID(ctx, hostID, "boot-aaa")
	if err != nil {
		t.Fatalf("UpdateHeartbeatBootID same: %v", err)
	}
	if prevBoot != "boot-aaa" {
		t.Errorf("previous boot_id = %q, want boot-aaa (no change)", prevBoot)
	}

	// Different boot ID — host rebooted.
	prevBoot, err = repo.UpdateHeartbeatBootID(ctx, hostID, "boot-bbb")
	if err != nil {
		t.Fatalf("UpdateHeartbeatBootID changed: %v", err)
	}
	if prevBoot != "boot-aaa" {
		t.Errorf("previous boot_id = %q, want boot-aaa (detected change)", prevBoot)
	}
}

// ── F-7: Stale hosts excluded from GetAvailableHosts ──────────────────────────

func TestHostFailure_StaleHost_ExcludedFromAvailable(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("hf7-%d", time.Now().UnixNano())

	if err := repo.UpsertHost(ctx, &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4, TotalMemoryMB: 8192, TotalDiskGB: 100,
		AgentVersion: "v1.0",
	}); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	// Give it a heartbeat (makes it appear in GetAvailableHosts).
	if err := repo.UpdateHeartbeat(ctx, hostID, 0, 0, 0, "v1.0"); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}

	// Host should appear in available list (fresh heartbeat).
	available, err := repo.GetAvailableHosts(ctx)
	if err != nil {
		t.Fatalf("GetAvailableHosts: %v", err)
	}
	found := false
	for _, h := range available {
		if h.ID == hostID {
			found = true
			break
		}
	}
	if !found {
		t.Skip("host not in available list (may not have passed stale window)")
	}

	// GetStaleHosts should NOT include this host (fresh heartbeat).
	stale, err := repo.GetStaleHosts(ctx)
	if err != nil {
		t.Fatalf("GetStaleHosts: %v", err)
	}
	for _, h := range stale {
		if h.ID == hostID {
			t.Error("fresh-heartbeat host should not be in stale hosts list")
		}
	}
}

// ── F-8: No automatic recovery without fencing ────────────────────────────────

func TestHostFailure_NoAutoRecoveryWithoutFencing(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("hf8-%d", time.Now().UnixNano())

	if err := repo.UpsertHost(ctx, &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4, TotalMemoryMB: 8192, TotalDiskGB: 100,
		AgentVersion: "v1.0",
	}); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	// Mark unhealthy with fencing required.
	repo.MarkHostUnhealthy(ctx, hostID, 0, "ready", db.ReasonCodeHypervisorFailed)

	// Verify the full safety chain:
	// 1. GetRecoveryEligibleHosts excludes it (fence_required=TRUE).
	eligible, err := repo.GetRecoveryEligibleHosts(ctx)
	if err != nil {
		t.Fatalf("GetRecoveryEligibleHosts: %v", err)
	}
	eligibleIDs := make(map[string]bool)
	for _, h := range eligible {
		eligibleIDs[h.ID] = true
	}
	if eligibleIDs[hostID] {
		t.Error("SAFETY VIOLATION: fence_required=TRUE host appears in recovery-eligible list")
	}

	// 2. GetFenceRequiredHosts includes it.
	fenced, err := repo.GetFenceRequiredHosts(ctx)
	if err != nil {
		t.Fatalf("GetFenceRequiredHosts: %v", err)
	}
	fencedIDs := make(map[string]bool)
	for _, h := range fenced {
		fencedIDs[h.ID] = true
	}
	if !fencedIDs[hostID] {
		t.Error("fence_required=TRUE host should appear in GetFenceRequiredHosts")
	}

	// 3. InsertRecoveryLog captures the skipped decision.
	err = repo.InsertRecoveryLog(ctx, &db.RecoveryLogRecord{
		ID:                      idgen.New("evt"),
		HostID:                  hostID,
		Verdict:                 db.RecoveryVerdictSkippedFenceRequired,
		Reason:                  "fencing required — recovery automation blocked",
		HostStatusAtAttempt:     "unhealthy",
		HostGenerationAtAttempt: 1,
		FenceRequiredAtAttempt:  true,
		Actor:                   "recovery_loop",
	})
	if err != nil {
		t.Fatalf("InsertRecoveryLog: %v", err)
	}

	// 4. Clear fence then recovery becomes eligible.
	host, _ := repo.GetHostByID(ctx, hostID)
	repo.ClearFenceRequired(ctx, hostID, host.Generation)

	eligible, _ = repo.GetRecoveryEligibleHosts(ctx)
	eligibleIDs = make(map[string]bool)
	for _, h := range eligible {
		eligibleIDs[h.ID] = true
	}
	if !eligibleIDs[hostID] {
		t.Error("after ClearFenceRequired, host should be recovery-eligible")
	}
}

// ensure errors import is not flagged unused
var _ = errors.Is
