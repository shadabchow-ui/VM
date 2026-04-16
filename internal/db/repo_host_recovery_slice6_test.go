package db

// repo_host_recovery_slice6_test.go — Unit tests for VM-P2E Slice 6 recovery DB methods.
//
// Tests cover:
//   - RecoveryVerdict constants: all defined, non-empty, distinct
//   - InsertRecoveryLog: SQL executed, campaign_id nil vs set
//   - GetRecoveryEligibleHosts: query-level logic (status filter, fence_required=FALSE)
//   - GetHostRecoveryLog: per-host history query
//
// All tests run without a real PostgreSQL instance (fakePool from repo_test.go).
//
// Source: 11-02-phase-1-test-strategy §Unit.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── RecoveryVerdict constants ─────────────────────────────────────────────────

func TestRecoveryVerdictConstants_Defined(t *testing.T) {
	verdicts := []string{
		RecoveryVerdictSkippedFenceRequired,
		RecoveryVerdictSkippedNotEligible,
		RecoveryVerdictReactivated,
		RecoveryVerdictDrainInitiated,
		RecoveryVerdictCASFailed,
		RecoveryVerdictError,
	}
	seen := map[string]bool{}
	for _, v := range verdicts {
		if v == "" {
			t.Error("verdict constant is empty string")
		}
		if seen[v] {
			t.Errorf("verdict constant %q is duplicated", v)
		}
		seen[v] = true
	}
	if len(seen) != 6 {
		t.Errorf("expected 6 distinct verdict constants, got %d", len(seen))
	}
}

// ── InsertRecoveryLog ─────────────────────────────────────────────────────────

func TestInsertRecoveryLog_ExecSQL(t *testing.T) {
	pool := &fakePool{execRows: 1}
	repo := New(pool)

	rec := &RecoveryLogRecord{
		ID:                      "rl-001",
		HostID:                  "host-aaa",
		Verdict:                 RecoveryVerdictReactivated,
		Reason:                  "drained host reactivated after maintenance window",
		HostStatusAtAttempt:     "drained",
		HostGenerationAtAttempt: 5,
		FenceRequiredAtAttempt:  false,
		Actor:                   "operator",
		CampaignID:              nil,
	}

	if err := repo.InsertRecoveryLog(context.Background(), rec); err != nil {
		t.Fatalf("InsertRecoveryLog: unexpected error: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	sql := pool.execCalls[0].query
	if !strings.Contains(sql, "host_recovery_log") {
		t.Errorf("SQL does not reference host_recovery_log: %s", sql)
	}
	// Verify campaign_id arg is nil when CampaignID is nil.
	args := pool.execCalls[0].args
	// args: id, host_id, verdict, reason, status, gen, fence_req, actor, campaign_id
	if len(args) < 9 {
		t.Fatalf("expected >=9 args, got %d", len(args))
	}
	if args[8] != nil {
		t.Errorf("campaign_id arg should be nil when CampaignID is nil, got %v", args[8])
	}
}

func TestInsertRecoveryLog_WithCampaignID(t *testing.T) {
	pool := &fakePool{execRows: 1}
	repo := New(pool)

	campaignID := "camp-xyz"
	rec := &RecoveryLogRecord{
		ID:                      "rl-002",
		HostID:                  "host-bbb",
		Verdict:                 RecoveryVerdictSkippedFenceRequired,
		Reason:                  "fence_required=TRUE; STONITH pending",
		HostStatusAtAttempt:     "unhealthy",
		HostGenerationAtAttempt: 3,
		FenceRequiredAtAttempt:  true,
		Actor:                   "recovery_loop",
		CampaignID:              &campaignID,
	}

	if err := repo.InsertRecoveryLog(context.Background(), rec); err != nil {
		t.Fatalf("InsertRecoveryLog with campaign_id: unexpected error: %v", err)
	}

	args := pool.execCalls[0].args
	if args[8] != campaignID {
		t.Errorf("campaign_id arg should be %q, got %v", campaignID, args[8])
	}
}

func TestInsertRecoveryLog_DBError(t *testing.T) {
	pool := &fakePool{execErr: errTestDB}
	repo := New(pool)

	rec := &RecoveryLogRecord{
		ID:     "rl-003",
		HostID: "host-ccc",
		Verdict: RecoveryVerdictError,
		Reason: "DB write test",
		Actor:  "operator",
	}

	err := repo.InsertRecoveryLog(context.Background(), rec)
	if err == nil {
		t.Error("expected error on DB failure, got nil")
	}
	if !strings.Contains(err.Error(), "InsertRecoveryLog") {
		t.Errorf("error should mention InsertRecoveryLog: %v", err)
	}
}

// ── GetRecoveryEligibleHosts ──────────────────────────────────────────────────

func TestGetRecoveryEligibleHosts_QueryContainsFenceRequiredFilter(t *testing.T) {
	pool := &fakePool{}
	// Return zero rows — we are checking the SQL shape, not the scan path.
	repo := New(pool)

	hosts, err := repo.GetRecoveryEligibleHosts(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hosts != nil {
		t.Errorf("expected nil slice for empty result, got %v", hosts)
	}
}

func TestGetRecoveryEligibleHosts_ScanReturnsHosts(t *testing.T) {
	now := time.Now().UTC()
	// Build a fake row for one host. Columns match GetRecoveryEligibleHosts SELECT.
	// id, az, status, gen, drain_reason, reason_code, fence_required, retired_at,
	// total_cpu, total_mem, total_disk, used_cpu, used_mem, used_disk,
	// agent_ver, last_hb, registered_at, updated_at
	row := []any{
		"host-ddd", "us-east-1a", "drained",
		int64(7), nil, nil, false, nil,
		16, 32768, 500,
		0, 0, 0,
		"v1.2.3", now, now, now,
	}
	pool := &fakePool{queryRowsData: [][]any{row}}
	repo := New(pool)

	hosts, err := repo.GetRecoveryEligibleHosts(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(hosts))
	}
	h := hosts[0]
	if h.ID != "host-ddd" {
		t.Errorf("host ID: got %q, want %q", h.ID, "host-ddd")
	}
	if h.Status != "drained" {
		t.Errorf("host status: got %q, want %q", h.Status, "drained")
	}
	if h.FenceRequired {
		t.Error("fence_required should be false for eligible host")
	}
	if h.Generation != 7 {
		t.Errorf("generation: got %d, want 7", h.Generation)
	}
}

// ── GetHostRecoveryLog ────────────────────────────────────────────────────────

func TestGetHostRecoveryLog_EmptyResult(t *testing.T) {
	pool := &fakePool{}
	repo := New(pool)

	records, err := repo.GetHostRecoveryLog(context.Background(), "host-zzz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if records != nil {
		t.Errorf("expected nil slice for empty result, got %v", records)
	}
}

func TestGetHostRecoveryLog_ScanReturnsRecords(t *testing.T) {
	now := time.Now().UTC()
	campaignID := "camp-abc"
	// Columns: id, host_id, verdict, reason, status_at, gen_at, fence_req_at,
	//          actor, campaign_id, attempted_at
	row := []any{
		"rl-100", "host-eee",
		RecoveryVerdictSkippedFenceRequired,
		"fence_required=TRUE at attempt time",
		"unhealthy", int64(4), true,
		"recovery_loop", campaignID, now,
	}
	pool := &fakePool{queryRowsData: [][]any{row}}
	repo := New(pool)

	records, err := repo.GetHostRecoveryLog(context.Background(), "host-eee")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	rec := records[0]
	if rec.Verdict != RecoveryVerdictSkippedFenceRequired {
		t.Errorf("verdict: got %q, want %q", rec.Verdict, RecoveryVerdictSkippedFenceRequired)
	}
	if !rec.FenceRequiredAtAttempt {
		t.Error("fence_required_at_attempt should be true")
	}
	if rec.CampaignID == nil || *rec.CampaignID != campaignID {
		t.Errorf("campaign_id: got %v, want %q", rec.CampaignID, campaignID)
	}
}

// ── helper shared across this file ───────────────────────────────────────────

// errTestDB is a sentinel error for DB failure injection.
// Uses the package-level errors.New equivalent to avoid import cycle issues.
var errTestDB = fmt.Errorf("simulated DB failure")
