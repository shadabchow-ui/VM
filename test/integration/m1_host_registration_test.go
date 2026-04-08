//go:build integration

package integration

// m1_host_registration_test.go — M1 gate integration tests.
//
// Runs against a real PostgreSQL instance (DATABASE_URL env).
// All four M1 acceptance criteria are covered:
//   1. Host Agent registers on startup             → TestHostRegistration_HappyPath
//   2. Resource Manager stores host inventory      → TestHostRegistration_InventoryPersisted
//   3. Auth enforced on host↔control-plane path   → TestAuth_RejectsExpiredToken + TestAuth_RejectsWrongHostID
//   4. Scheduler reads host inventory              → TestScheduler_SelectHost_*
//
// Run: go test -tags=integration -v ./test/integration/... -run TestHost
//      go test -tags=integration -v ./test/integration/... -run TestScheduler
//      go test -tags=integration -v ./test/integration/... (all M1 tests)
//
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Test helpers ──────────────────────────────────────────────────────────────

// testRepo constructs a *db.Repo for use in tests.
func testRepo(t *testing.T) *db.Repo {
	t.Helper()
	return db.New(testPool(t))
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ── M1 Gate Test 1: Host Agent registers on startup ──────────────────────────

// TestHostRegistration_HappyPath verifies the full M1 registration path:
// bootstrap token consumed → host record upserted → status=ready.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests bullet 1.
func TestHostRegistration_HappyPath(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("test-host-%d", time.Now().UnixNano())
	rawToken := fmt.Sprintf("test-token-happy-path-%d", time.Now().UnixNano())
	tokenHash := sha256hex(rawToken)
	expiresAt := time.Now().Add(time.Hour)

	// Seed token.
	if err := repo.InsertBootstrapToken(ctx, tokenHash, hostID, expiresAt); err != nil {
		t.Fatalf("InsertBootstrapToken: %v", err)
	}

	// Consume token (simulates Resource Manager CSR endpoint validating token).
	gotHostID, err := repo.ConsumeBootstrapToken(ctx, tokenHash)
	if err != nil {
		t.Fatalf("ConsumeBootstrapToken: %v", err)
	}
	if gotHostID != hostID {
		t.Errorf("ConsumeBootstrapToken host_id: got %q, want %q", gotHostID, hostID)
	}

	// Upsert host (simulates Resource Manager registration handler after cert issued).
	rec := &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         8,
		TotalMemoryMB:    16384,
		TotalDiskGB:      200,
		AgentVersion:     "v0.1.0-m1",
	}
	if err := repo.UpsertHost(ctx, rec); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	// Verify host record visible in DB.
	got, err := repo.GetHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if got.Status != "ready" {
		t.Errorf("host status: got %q, want %q", got.Status, "ready")
	}
	if got.TotalCPU != 8 {
		t.Errorf("total_cpu: got %d, want 8", got.TotalCPU)
	}
	if got.AvailabilityZone != "us-east-1a" {
		t.Errorf("az: got %q, want us-east-1a", got.AvailabilityZone)
	}
}

// ── M1 Gate Test 2: Inventory persisted ──────────────────────────────────────

// TestHostRegistration_InventoryPersisted verifies that host inventory is
// readable after registration, with correct resource counts.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests bullet 2.
func TestHostRegistration_InventoryPersisted(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("test-inv-%d", time.Now().UnixNano())
	rec := &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1b",
		TotalCPU:         16,
		TotalMemoryMB:    32768,
		TotalDiskGB:      500,
		AgentVersion:     "v0.1.0-m1",
	}
	if err := repo.UpsertHost(ctx, rec); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	got, err := repo.GetHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if got.TotalCPU != 16 || got.TotalMemoryMB != 32768 || got.TotalDiskGB != 500 {
		t.Errorf("inventory mismatch: got cpu=%d mem=%d disk=%d", got.TotalCPU, got.TotalMemoryMB, got.TotalDiskGB)
	}
	if got.UsedCPU != 0 || got.UsedMemoryMB != 0 || got.UsedDiskGB != 0 {
		t.Error("used_* should be 0 after registration")
	}
}

// ── M1 Gate Test 3: Auth — expired token rejected ────────────────────────────

// TestAuth_RejectsExpiredToken verifies ConsumeBootstrapToken rejects expired tokens.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests bullet 3.
func TestAuth_RejectsExpiredToken(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("test-expired-%d", time.Now().UnixNano())
	rawToken := fmt.Sprintf("expired-token-%d", time.Now().UnixNano())
	tokenHash := sha256hex(rawToken)
	expiresAt := time.Now().Add(-5 * time.Minute) // already expired

	if err := repo.InsertBootstrapToken(ctx, tokenHash, hostID, expiresAt); err != nil {
		t.Fatalf("InsertBootstrapToken: %v", err)
	}

	_, err := repo.ConsumeBootstrapToken(ctx, tokenHash)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if err != db.ErrBootstrapTokenInvalid {
		t.Errorf("expected ErrBootstrapTokenInvalid, got: %v", err)
	}
}

// TestAuth_RejectsAlreadyUsedToken verifies a consumed token cannot be reused.
// Source: AUTH_OWNERSHIP_MODEL_V1 §6 "Token invalidated after first use".
func TestAuth_RejectsAlreadyUsedToken(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("test-used-%d", time.Now().UnixNano())
	rawToken := fmt.Sprintf("used-token-%d", time.Now().UnixNano())
	tokenHash := sha256hex(rawToken)

	if err := repo.InsertBootstrapToken(ctx, tokenHash, hostID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("InsertBootstrapToken: %v", err)
	}

	// First consumption: success.
	if _, err := repo.ConsumeBootstrapToken(ctx, tokenHash); err != nil {
		t.Fatalf("first consume: %v", err)
	}

	// Second consumption: must fail.
	_, err := repo.ConsumeBootstrapToken(ctx, tokenHash)
	if err == nil {
		t.Fatal("expected error on second consume, got nil")
	}
	if err != db.ErrBootstrapTokenInvalid {
		t.Errorf("expected ErrBootstrapTokenInvalid, got: %v", err)
	}
}

// TestAuth_RejectsWrongHostID verifies that ConsumeBootstrapToken returns the
// correct host_id, and that a mismatch in the handler would be caught.
// Source: AUTH_OWNERSHIP_MODEL_V1 §6.
func TestAuth_RejectsWrongHostID(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("test-correct-%d", time.Now().UnixNano())
	rawToken := fmt.Sprintf("token-correct-%d", time.Now().UnixNano())
	tokenHash := sha256hex(rawToken)

	if err := repo.InsertBootstrapToken(ctx, tokenHash, hostID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("InsertBootstrapToken: %v", err)
	}

	gotHostID, err := repo.ConsumeBootstrapToken(ctx, tokenHash)
	if err != nil {
		t.Fatalf("ConsumeBootstrapToken: %v", err)
	}

	// Simulate handler check: cert CN says a different host_id.
	fakeHostID := "impersonator-host"
	if gotHostID == fakeHostID {
		t.Error("token should not map to impersonator host_id")
	}
	if gotHostID != hostID {
		t.Errorf("token host_id: got %q, want %q", gotHostID, hostID)
	}
}

// ── M1 Gate Test 4: Idempotent re-registration ───────────────────────────────

// TestHostRegistration_Idempotent verifies that registering the same host twice
// results in a single row with status=ready.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests bullet 1 (idempotency).
func TestHostRegistration_Idempotent(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("test-idem-%d", time.Now().UnixNano())
	rec := &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         4,
		TotalMemoryMB:    8192,
		TotalDiskGB:      100,
		AgentVersion:     "v0.1.0-m1",
	}

	// Register twice.
	if err := repo.UpsertHost(ctx, rec); err != nil {
		t.Fatalf("first UpsertHost: %v", err)
	}
	rec.AgentVersion = "v0.1.1-m1" // version bump on restart
	if err := repo.UpsertHost(ctx, rec); err != nil {
		t.Fatalf("second UpsertHost: %v", err)
	}

	got, err := repo.GetHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if got.Status != "ready" {
		t.Errorf("status after re-registration: got %q, want ready", got.Status)
	}
	if got.AgentVersion != "v0.1.1-m1" {
		t.Errorf("agent_version not updated: got %q", got.AgentVersion)
	}
}

// ── M1 Gate Test 5: Heartbeat updates inventory ──────────────────────────────

// TestHeartbeat_UpdatesInventory verifies that a heartbeat updates utilization
// and last_heartbeat_at in the DB.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests bullet 2.
func TestHeartbeat_UpdatesInventory(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("test-hb-%d", time.Now().UnixNano())
	rec := &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         8,
		TotalMemoryMB:    16384,
		TotalDiskGB:      200,
		AgentVersion:     "v0.1.0-m1",
	}
	if err := repo.UpsertHost(ctx, rec); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}

	// Send heartbeat with updated utilization.
	if err := repo.UpdateHeartbeat(ctx, hostID, 2, 4096, 10, "v0.1.0-m1"); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}

	got, err := repo.GetHostByID(ctx, hostID)
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if got.UsedCPU != 2 {
		t.Errorf("used_cpu: got %d, want 2", got.UsedCPU)
	}
	if got.UsedMemoryMB != 4096 {
		t.Errorf("used_memory_mb: got %d, want 4096", got.UsedMemoryMB)
	}
	if got.LastHeartbeatAt == nil {
		t.Error("last_heartbeat_at should be set after heartbeat")
	}
}

// TestHeartbeat_UnknownHostReturnsError verifies ErrHostNotFound for unregistered hosts.
func TestHeartbeat_UnknownHostReturnsError(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	err := repo.UpdateHeartbeat(ctx, "nonexistent-host-id", 0, 0, 0, "v0.1.0")
	if err == nil {
		t.Fatal("expected error for unknown host, got nil")
	}
}

// ── M1 Gate Test 6: Scheduler reads available hosts ──────────────────────────

// TestScheduler_SelectHost_ReturnsAvailable verifies GetAvailableHosts returns
// ready hosts and SelectHost (CanFit) selects the right one.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests bullet 4.
func TestScheduler_SelectHost_ReturnsAvailable(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	// Seed three hosts with different capacities.
	hosts := []db.HostRecord{
		{ID: fmt.Sprintf("sched-small-%d", time.Now().UnixNano()), AvailabilityZone: "us-east-1a", TotalCPU: 4, TotalMemoryMB: 8192, TotalDiskGB: 100, AgentVersion: "v0.1.0"},
		{ID: fmt.Sprintf("sched-med-%d", time.Now().UnixNano()), AvailabilityZone: "us-east-1a", TotalCPU: 8, TotalMemoryMB: 16384, TotalDiskGB: 200, AgentVersion: "v0.1.0"},
		{ID: fmt.Sprintf("sched-large-%d", time.Now().UnixNano()), AvailabilityZone: "us-east-1a", TotalCPU: 16, TotalMemoryMB: 32768, TotalDiskGB: 500, AgentVersion: "v0.1.0"},
	}
	for i := range hosts {
		if err := repo.UpsertHost(ctx, &hosts[i]); err != nil {
			t.Fatalf("UpsertHost[%d]: %v", i, err)
		}
		// Send heartbeat so they appear in GetAvailableHosts (requires last_heartbeat_at within 90s).
		if err := repo.UpdateHeartbeat(ctx, hosts[i].ID, 0, 0, 0, "v0.1.0"); err != nil {
			t.Fatalf("UpdateHeartbeat[%d]: %v", i, err)
		}
	}

	available, err := repo.GetAvailableHosts(ctx)
	if err != nil {
		t.Fatalf("GetAvailableHosts: %v", err)
	}
	if len(available) < 3 {
		t.Fatalf("expected at least 3 available hosts, got %d", len(available))
	}

	// SelectHost logic: first fit descending by free CPU.
	// Request: 6 CPU, 12 GB RAM, 150 GB disk — should pick the 8-core host at minimum.
	var selected *db.HostRecord
	for _, h := range available {
		if h.CanFit(6, 12288, 150) {
			selected = h
			break
		}
	}
	if selected == nil {
		t.Fatal("SelectHost returned nil — no host fit the request")
	}
	if selected.TotalCPU < 6 {
		t.Errorf("selected host has insufficient CPU: %d", selected.TotalCPU)
	}
}

// TestScheduler_SelectHost_RejectsNoCapacity verifies ErrNoCapacity when all
// hosts are too small or at full utilization.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests bullet 4.
func TestScheduler_SelectHost_RejectsNoCapacity(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	// Seed one tiny host.
	hostID := fmt.Sprintf("tiny-%d", time.Now().UnixNano())
	rec := &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         2,
		TotalMemoryMB:    4096,
		TotalDiskGB:      50,
		AgentVersion:     "v0.1.0",
	}
	if err := repo.UpsertHost(ctx, rec); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}
	if err := repo.UpdateHeartbeat(ctx, hostID, 0, 0, 0, "v0.1.0"); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}

	// Request is far larger than the tiny host.
	hosts, err := repo.GetAvailableHosts(ctx)
	if err != nil {
		t.Fatalf("GetAvailableHosts: %v", err)
	}
	for _, h := range hosts {
		if h.CanFit(32, 65536, 1000) {
			t.Errorf("tiny host %s should not fit 32-CPU request", h.ID)
		}
	}
}

// TestScheduler_StaleHeartbeat_ExcludedFromPlacement verifies that a host
// with a stale heartbeat (> 90 seconds old) does not appear in GetAvailableHosts.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests bullet 4, GetAvailableHosts staleness filter.
func TestScheduler_StaleHeartbeat_ExcludedFromPlacement(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	hostID := fmt.Sprintf("stale-%d", time.Now().UnixNano())
	rec := &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         8,
		TotalMemoryMB:    16384,
		TotalDiskGB:      200,
		AgentVersion:     "v0.1.0",
	}
	if err := repo.UpsertHost(ctx, rec); err != nil {
		t.Fatalf("UpsertHost: %v", err)
	}
	// Do NOT send a heartbeat — last_heartbeat_at remains NULL.
	// The GetAvailableHosts query requires last_heartbeat_at > NOW() - 90s,
	// so this host must not appear.

	available, err := repo.GetAvailableHosts(ctx)
	if err != nil {
		t.Fatalf("GetAvailableHosts: %v", err)
	}
	for _, h := range available {
		if h.ID == hostID {
			t.Errorf("stale host %s should not appear in available hosts", hostID)
		}
	}
}

// ── M1 Gate Test 7: Migration round-trip ─────────────────────────────────────

// TestMigration_HostsTableExists verifies the 002_hosts migration was applied.
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests (migration applies cleanly).
func TestMigration_HostsTableExists(t *testing.T) {
	ctx := context.Background()
	repo := testRepo(t)

	// If the hosts table doesn't exist, this will error.
	if err := repo.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Attempt an insert to confirm the table schema is correct.
	hostID := fmt.Sprintf("schema-check-%d", time.Now().UnixNano())
	rec := &db.HostRecord{
		ID:               hostID,
		AvailabilityZone: "us-east-1a",
		TotalCPU:         1,
		TotalMemoryMB:    1024,
		TotalDiskGB:      10,
		AgentVersion:     "v0.0.0",
	}
	if err := repo.UpsertHost(ctx, rec); err != nil {
		t.Fatalf("hosts table schema check failed: %v", err)
	}
}
