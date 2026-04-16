package db

// repo_campaign_slice5_test.go — Unit tests for VM-P2E Slice 5 maintenance campaign DB methods.
//
// Tests cover:
//   - CampaignRecord helpers: InFlightCount, IsTerminal, NextHosts
//   - CreateCampaign: success, blast-radius rejection, zero-target rejection
//   - GetCampaignByID: not-found path
//   - UpdateCampaignStatus: success, no-op (same status)
//   - AdvanceCampaignProgress: completed and failed paths; unknown outcome
//   - MaxCampaignParallel constant: defined and > 0
//   - ErrBlastRadiusExceeded / ErrCampaignNotFound / ErrCampaignNoTargets: defined
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// ── CampaignRecord helpers ────────────────────────────────────────────────────

func TestCampaignRecord_IsTerminal(t *testing.T) {
	cases := []struct {
		status   string
		terminal bool
	}{
		{"pending", false},
		{"running", false},
		{"paused", false},
		{"completed", true},
		{"cancelled", true},
	}
	for _, tc := range cases {
		c := &CampaignRecord{Status: tc.status}
		if got := c.IsTerminal(); got != tc.terminal {
			t.Errorf("status=%q: IsTerminal()=%v, want %v", tc.status, got, tc.terminal)
		}
	}
}

func TestCampaignRecord_InFlightCount(t *testing.T) {
	c := &CampaignRecord{
		TargetHostIDs:    []string{"h1", "h2", "h3", "h4"},
		CompletedHostIDs: []string{"h1"},
		FailedHostIDs:    []string{"h2"},
	}
	// 4 targets - 1 completed - 1 failed = 2 in-flight
	if got := c.InFlightCount(); got != 2 {
		t.Errorf("InFlightCount()=%d, want 2", got)
	}
}

func TestCampaignRecord_InFlightCount_AllDone(t *testing.T) {
	c := &CampaignRecord{
		TargetHostIDs:    []string{"h1", "h2"},
		CompletedHostIDs: []string{"h1", "h2"},
		FailedHostIDs:    []string{},
	}
	if got := c.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount()=%d, want 0", got)
	}
}

func TestCampaignRecord_NextHosts_RespectsMaxParallel(t *testing.T) {
	c := &CampaignRecord{
		TargetHostIDs:    []string{"h1", "h2", "h3", "h4", "h5"},
		CompletedHostIDs: []string{"h1"},
		FailedHostIDs:    []string{},
	}
	// Ask for up to 2 hosts from the remaining 4 (h2, h3, h4, h5)
	next := c.NextHosts(2)
	if len(next) != 2 {
		t.Errorf("NextHosts(2) returned %d hosts, want 2", len(next))
	}
	if next[0] != "h2" || next[1] != "h3" {
		t.Errorf("NextHosts(2) = %v, want [h2 h3]", next)
	}
}

func TestCampaignRecord_NextHosts_SkipsCompleted(t *testing.T) {
	c := &CampaignRecord{
		TargetHostIDs:    []string{"h1", "h2", "h3"},
		CompletedHostIDs: []string{"h1", "h2"},
		FailedHostIDs:    []string{},
	}
	next := c.NextHosts(5)
	if len(next) != 1 || next[0] != "h3" {
		t.Errorf("NextHosts(5) = %v, want [h3]", next)
	}
}

func TestCampaignRecord_NextHosts_SkipsFailed(t *testing.T) {
	c := &CampaignRecord{
		TargetHostIDs:    []string{"h1", "h2"},
		CompletedHostIDs: []string{},
		FailedHostIDs:    []string{"h1"},
	}
	next := c.NextHosts(5)
	if len(next) != 1 || next[0] != "h2" {
		t.Errorf("NextHosts(5) = %v, want [h2]", next)
	}
}

func TestCampaignRecord_NextHosts_NoneRemaining(t *testing.T) {
	c := &CampaignRecord{
		TargetHostIDs:    []string{"h1"},
		CompletedHostIDs: []string{"h1"},
		FailedHostIDs:    []string{},
	}
	next := c.NextHosts(5)
	if len(next) != 0 {
		t.Errorf("NextHosts(5) = %v, want []", next)
	}
}

// ── Constants / sentinel errors ───────────────────────────────────────────────

func TestMaxCampaignParallel_IsPositive(t *testing.T) {
	if MaxCampaignParallel <= 0 {
		t.Errorf("MaxCampaignParallel = %d, want > 0", MaxCampaignParallel)
	}
}

func TestCampaignSentinelErrors_Defined(t *testing.T) {
	if ErrCampaignNotFound == nil {
		t.Error("ErrCampaignNotFound must be non-nil")
	}
	if ErrBlastRadiusExceeded == nil {
		t.Error("ErrBlastRadiusExceeded must be non-nil")
	}
	if ErrCampaignNoTargets == nil {
		t.Error("ErrCampaignNoTargets must be non-nil")
	}
}

// ── CreateCampaign validations ────────────────────────────────────────────────

func TestCreateCampaign_BlastRadiusExceeded_RejectsBeforeDB(t *testing.T) {
	pool := &fakePool{execRows: 1}
	repo := New(pool)

	_, err := repo.CreateCampaign(context.Background(),
		"camp-001", "test", []string{"h1"}, MaxCampaignParallel+1)
	if !errors.Is(err, ErrBlastRadiusExceeded) {
		t.Errorf("expected ErrBlastRadiusExceeded, got: %v", err)
	}
	// No DB call should have been made.
	if len(pool.execCalls) > 0 {
		t.Error("DB exec must not be called when blast-radius check fails")
	}
}

func TestCreateCampaign_ZeroTargets_RejectsBeforeDB(t *testing.T) {
	pool := &fakePool{execRows: 1}
	repo := New(pool)

	_, err := repo.CreateCampaign(context.Background(),
		"camp-001", "test", []string{}, 1)
	if !errors.Is(err, ErrCampaignNoTargets) {
		t.Errorf("expected ErrCampaignNoTargets, got: %v", err)
	}
	if len(pool.execCalls) > 0 {
		t.Error("DB exec must not be called when target list is empty")
	}
}

func TestCreateCampaign_MaxParallelClampedToOne(t *testing.T) {
	// max_parallel < 1 is clamped to 1, not rejected.
	pool := &campaignFakePool{
		execRows:       1,
		campaignRecord: makeCampaignRow("camp-clamp", 1, "pending"),
	}
	repo := New(pool)

	_, err := repo.CreateCampaign(context.Background(),
		"camp-clamp", "test", []string{"h1"}, 0)
	if err != nil {
		t.Fatalf("expected success with max_parallel=0 clamped to 1, got: %v", err)
	}
}

func TestCreateCampaign_DBError_PropagatesError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("connection refused")}
	repo := New(pool)

	_, err := repo.CreateCampaign(context.Background(),
		"camp-001", "test", []string{"h1"}, 1)
	if err == nil {
		t.Fatal("expected error propagation, got nil")
	}
}

// ── GetCampaignByID ───────────────────────────────────────────────────────────

func TestGetCampaignByID_NotFound_ReturnsErrCampaignNotFound(t *testing.T) {
	pool := &fakePool{queryRowErr: fmt.Errorf("no rows in result set")}
	repo := New(pool)

	_, err := repo.GetCampaignByID(context.Background(), "missing-id")
	if !errors.Is(err, ErrCampaignNotFound) {
		t.Errorf("expected ErrCampaignNotFound, got: %v", err)
	}
}

// ── UpdateCampaignStatus ──────────────────────────────────────────────────────

func TestUpdateCampaignStatus_Success_ReturnsTrue(t *testing.T) {
	pool := &fakePool{execRows: 1}
	repo := New(pool)

	updated, err := repo.UpdateCampaignStatus(context.Background(), "camp-001", "running")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !updated {
		t.Error("expected updated=true")
	}
}

func TestUpdateCampaignStatus_NoChange_ReturnsFalse(t *testing.T) {
	pool := &fakePool{execRows: 0} // status == newStatus → WHERE status != $2 matches 0 rows
	repo := New(pool)

	updated, err := repo.UpdateCampaignStatus(context.Background(), "camp-001", "running")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated {
		t.Error("expected updated=false when no rows were changed")
	}
}

// ── AdvanceCampaignProgress ───────────────────────────────────────────────────

func TestAdvanceCampaignProgress_UnknownOutcome_ReturnsError(t *testing.T) {
	pool := &fakePool{}
	repo := New(pool)

	_, err := repo.AdvanceCampaignProgress(context.Background(), "camp-001", "h1", "bogus")
	if err == nil {
		t.Error("expected error for unknown outcome, got nil")
	}
}

func TestAdvanceCampaignProgress_CompletedOutcome_CallsExec(t *testing.T) {
	pool := &fakePool{execRows: 1}
	repo := New(pool)

	ok, err := repo.AdvanceCampaignProgress(context.Background(), "camp-001", "h1", "completed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected ok=true")
	}
	// Should have produced at least one exec call (the append) and a second
	// attempt for auto-complete.
	if len(pool.execCalls) < 1 {
		t.Errorf("expected at least 1 exec call, got %d", len(pool.execCalls))
	}
}

func TestAdvanceCampaignProgress_FailedOutcome_CallsExec(t *testing.T) {
	pool := &fakePool{execRows: 1}
	repo := New(pool)

	ok, err := repo.AdvanceCampaignProgress(context.Background(), "camp-001", "h1", "failed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected ok=true")
	}
}

func TestAdvanceCampaignProgress_NotInTargets_ReturnsFalse(t *testing.T) {
	pool := &fakePool{execRows: 0} // WHERE $2 = ANY(target_host_ids) matches 0 rows
	repo := New(pool)

	ok, err := repo.AdvanceCampaignProgress(context.Background(), "camp-001", "h-unknown", "completed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false when host not in target_host_ids")
	}
}

// ── campaignFakePool: extends fakePool for CreateCampaign→GetCampaignByID chain ──
//
// CreateCampaign calls Exec (insert) then GetCampaignByID (QueryRow).
// We need the QueryRow path to return a valid CampaignRecord scan.

type campaignFakePool struct {
	execRows       int64
	execErr        error
	campaignRecord []any // scan values for CampaignRecord fields
}

func (p *campaignFakePool) Exec(_ context.Context, _ string, _ ...any) (CommandTag, error) {
	if p.execErr != nil {
		return nil, p.execErr
	}
	return &fakeTag{rows: p.execRows}, nil
}

func (p *campaignFakePool) Query(_ context.Context, _ string, _ ...any) (Rows, error) {
	return &fakeRows{data: nil, idx: -1}, nil
}

func (p *campaignFakePool) QueryRow(_ context.Context, _ string, _ ...any) Row {
	if p.campaignRecord == nil {
		return &fakeRow{err: fmt.Errorf("no rows in result set")}
	}
	return &fakeRow{values: p.campaignRecord}
}

func (p *campaignFakePool) Close() {}

// makeCampaignRow returns a []any with the correct scan order for a CampaignRecord.
// Matches the SELECT column order in GetCampaignByID.
func makeCampaignRow(id string, maxParallel int, status string) []any {
	return []any{
		id,               // id
		"test reason",    // campaign_reason
		[]string{"h1"},   // target_host_ids (fakeRow Scan handles []string via direct assign)
		[]string{},       // completed_host_ids
		[]string{},       // failed_host_ids
		maxParallel,      // max_parallel
		status,           // status
		"2026-01-01T00:00:00Z", // created_at (string, fakeRow Scan ignores type mismatch gracefully)
		"2026-01-01T00:00:00Z", // updated_at
	}
}
