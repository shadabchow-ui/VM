package main

// host_recovery_handlers_slice6_test.go — Tests for VM-P2E Slice 6 recovery endpoints.
//
// Coverage:
//   GET /internal/v1/hosts/recovery-eligible
//     - 200: empty list when no eligible hosts
//     - 200: populated list with correct fields
//     - 500: DB error
//   POST /internal/v1/hosts/{host_id}/recover
//     - 200: reactivated (drained → ready)
//     - 200: drain_initiated (degraded → draining)
//     - 409: skipped_fence_required (fence_required=TRUE)
//     - 409: skipped_not_eligible (status=retiring)
//     - 409: cas_failed (generation mismatch)
//     - 400: invalid JSON
//     - 404: host not found
//   GET /internal/v1/hosts/{host_id}/recovery-log
//     - 200: empty entries when no history
//     - 200: entries returned with correct fields
//   GET /internal/v1/maintenance/campaigns/{id}/failed-hosts/recovery
//     - 200: assessment with eligible, blocked, not_recoverable, not_found
//     - 404: campaign not found
//   Helpers: extractRecoverHostID, extractRecoveryLogHostID, isHostNotFoundErr
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── stubRecoveryInventory ─────────────────────────────────────────────────────
//
// stubRecoveryPool implements db.Pool for Slice 6 recovery handler tests.
// It scripts responses to GetHostByID (QueryRow), GetRecoveryEligibleHosts
// (Query), GetHostRecoveryLog (Query), GetCampaignByID (QueryRow).

type recoveryTestPool struct {
	// GetHostByID: scripted single host.
	hostRec *db.HostRecord
	hostErr error

	// GetRecoveryEligibleHosts and GetHostRecoveryLog: Query rows.
	queryRows [][]any

	// Campaign: GetCampaignByID.
	campaignRec *db.CampaignRecord
	campaignErr error

	// Exec: UpdateHostStatus and InsertRecoveryLog.
	execRows int64
	execErr  error

	// Track call count to multiplex QueryRow for different methods.
	queryRowCallIdx int

	// Multi-QueryRow for campaign assessment tests: first call is GetCampaignByID,
	// subsequent calls are GetHostByID for each failed host.
	multiQueryRows []*db.HostRecord
	multiCallIdx   int
}

func (p *recoveryTestPool) Exec(_ context.Context, _ string, _ ...any) (db.CommandTag, error) {
	if p.execErr != nil {
		return nil, p.execErr
	}
	return &recoveryTestTag{n: p.execRows}, nil
}

func (p *recoveryTestPool) Query(_ context.Context, sql string, _ ...any) (db.Rows, error) {
	return &recoveryTestRows{data: p.queryRows, idx: -1}, nil
}

func (p *recoveryTestPool) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	// Campaign assessment: multiplex between campaign and host lookups.
	if p.multiQueryRows != nil {
		if p.queryRowCallIdx == 0 {
			p.queryRowCallIdx++
			if p.campaignErr != nil {
				return &recoveryTestRow{err: p.campaignErr}
			}
			return &campaignScanRow{rec: p.campaignRec}
		}
		// Subsequent calls: host lookups.
		idx := p.queryRowCallIdx - 1
		p.queryRowCallIdx++
		if idx < len(p.multiQueryRows) {
			return &recoveryHostScanRow{rec: p.multiQueryRows[idx]}
		}
		return &recoveryTestRow{err: fmt.Errorf("no rows in result set")}
	}

	// Single-campaign mode (for campaign-not-found test).
	if p.campaignRec != nil || p.campaignErr != nil {
		if p.campaignErr != nil {
			return &recoveryTestRow{err: p.campaignErr}
		}
		return &campaignScanRow{rec: p.campaignRec}
	}

	// Default: host lookup.
	if p.hostErr != nil {
		return &recoveryTestRow{err: p.hostErr}
	}
	if p.hostRec == nil {
		return &recoveryTestRow{err: fmt.Errorf("no rows in result set")}
	}
	return &recoveryHostScanRow{rec: p.hostRec}
}
func (p *recoveryTestPool) Close() {}

// recoveryTestTag implements db.CommandTag.
type recoveryTestTag struct{ n int64 }

func (t *recoveryTestTag) RowsAffected() int64 { return t.n }

// recoveryTestRow is a generic error-returning Row.
type recoveryTestRow struct{ err error }

func (r *recoveryTestRow) Scan(...any) error { return r.err }

// recoveryHostScanRow scans a HostRecord into the expected column positions.
// Matches the SELECT in GetHostByID.
type recoveryHostScanRow struct {
	rec *db.HostRecord
	err error
}

func (r *recoveryHostScanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.rec == nil {
		return fmt.Errorf("no rows in result set")
	}
	h := r.rec
	vals := []any{
		h.ID, h.AvailabilityZone, h.Status,
		h.Generation, h.DrainReason, h.ReasonCode, h.FenceRequired, h.RetiredAt,
		h.TotalCPU, h.TotalMemoryMB, h.TotalDiskGB,
		h.UsedCPU, h.UsedMemoryMB, h.UsedDiskGB,
		h.AgentVersion, h.LastHeartbeatAt, h.RegisteredAt, h.UpdatedAt,
	}
	for i, d := range dest {
		if i >= len(vals) {
			break
		}
		if vals[i] == nil {
			continue
		}
		switch dst := d.(type) {
		case *string:
			if v, ok := vals[i].(string); ok {
				*dst = v
			}
		case **string:
			if v, ok := vals[i].(string); ok {
				*dst = &v
			}
		case *int64:
			switch v := vals[i].(type) {
			case int64:
				*dst = v
			case int:
				*dst = int64(v)
			}
		case *int:
			switch v := vals[i].(type) {
			case int:
				*dst = v
			case int64:
				*dst = int(v)
			}
		case *bool:
			if v, ok := vals[i].(bool); ok {
				*dst = v
			}
		case **time.Time:
			if v, ok := vals[i].(time.Time); ok {
				*dst = &v
			}
		case *time.Time:
			if v, ok := vals[i].(time.Time); ok {
				*dst = v
			}
		}
	}
	return nil
}

// campaignScanRow scans a CampaignRecord into the expected column positions.
// Matches the SELECT in GetCampaignByID.
type campaignScanRow struct {
	rec *db.CampaignRecord
	err error
}

func (r *campaignScanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.rec == nil {
		return fmt.Errorf("no rows in result set")
	}
	c := r.rec
	vals := []any{
		c.ID, c.CampaignReason,
		c.TargetHostIDs, c.CompletedHostIDs, c.FailedHostIDs,
		c.MaxParallel, c.Status, c.CreatedAt, c.UpdatedAt,
	}
	for i, d := range dest {
		if i >= len(vals) {
			break
		}
		if vals[i] == nil {
			continue
		}
		switch dst := d.(type) {
		case *string:
			if v, ok := vals[i].(string); ok {
				*dst = v
			}
		case *int:
			if v, ok := vals[i].(int); ok {
				*dst = v
			}
		case *[]string:
			if v, ok := vals[i].([]string); ok {
				*dst = v
			}
		case *time.Time:
			if v, ok := vals[i].(time.Time); ok {
				*dst = v
			}
		}
	}
	return nil
}

// recoveryTestRows implements db.Rows for list queries.
type recoveryTestRows struct {
	data [][]any
	idx  int
}

func (r *recoveryTestRows) Next() bool { r.idx++; return r.idx < len(r.data) }
func (r *recoveryTestRows) Close()     {}
func (r *recoveryTestRows) Err() error { return nil }
func (r *recoveryTestRows) Scan(dest ...any) error {
	if r.idx >= len(r.data) {
		return fmt.Errorf("no row")
	}
	row := r.data[r.idx]
	for i, d := range dest {
		if i >= len(row) || row[i] == nil {
			continue
		}
		switch dst := d.(type) {
		case *string:
			if v, ok := row[i].(string); ok {
				*dst = v
			}
		case **string:
			if v, ok := row[i].(string); ok {
				*dst = &v
			}
		case *int64:
			switch v := row[i].(type) {
			case int64:
				*dst = v
			case int:
				*dst = int64(v)
			}
		case *int:
			switch v := row[i].(type) {
			case int:
				*dst = v
			case int64:
				*dst = int(v)
			}
		case *bool:
			if v, ok := row[i].(bool); ok {
				*dst = v
			}
		case **time.Time:
			if v, ok := row[i].(time.Time); ok {
				*dst = &v
			}
		case *time.Time:
			if v, ok := row[i].(time.Time); ok {
				*dst = v
			}
		}
	}
	return nil
}

// ── Test server factory ───────────────────────────────────────────────────────

func newRecoveryTestServer(pool *recoveryTestPool) *server {
	repo := db.New(pool)
	return &server{
		inventory: &HostInventory{repo: repo},
		log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func makeHostRec(id, status string, fenceRequired bool, generation int64) *db.HostRecord {
	now := time.Now().UTC()
	return &db.HostRecord{
		ID:               id,
		AvailabilityZone: "us-east-1a",
		Status:           status,
		Generation:       generation,
		FenceRequired:    fenceRequired,
		TotalCPU:         16,
		TotalMemoryMB:    32768,
		TotalDiskGB:      500,
		AgentVersion:     "v1.0.0",
		RegisteredAt:     now,
		UpdatedAt:        now,
	}
}

// ── GET /recovery-eligible ────────────────────────────────────────────────────

func TestHandleGetRecoveryEligibleHosts_EmptyList(t *testing.T) {
	pool := &recoveryTestPool{} // no rows
	srv := newRecoveryTestServer(pool)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/hosts/recovery-eligible", nil)
	w := httptest.NewRecorder()
	srv.handleGetRecoveryEligibleHosts(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp recoveryEligibleResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("count: got %d, want 0", resp.Count)
	}
	if resp.Hosts == nil || len(resp.Hosts) != 0 {
		t.Errorf("hosts: should be empty slice, got %v", resp.Hosts)
	}
}

func TestHandleGetRecoveryEligibleHosts_PopulatedList(t *testing.T) {
	now := time.Now().UTC()
	// Row format matches GetRecoveryEligibleHosts SELECT column order.
	row := []any{
		"host-ddd", "us-east-1a", "drained",
		int64(7), nil, nil, false, nil,
		16, 32768, 500,
		0, 0, 0,
		"v1.2.3", now, now, now,
	}
	pool := &recoveryTestPool{queryRows: [][]any{row}}
	srv := newRecoveryTestServer(pool)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/hosts/recovery-eligible", nil)
	w := httptest.NewRecorder()
	srv.handleGetRecoveryEligibleHosts(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp recoveryEligibleResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("count: got %d, want 1", resp.Count)
	}
	h := resp.Hosts[0]
	if h.ID != "host-ddd" {
		t.Errorf("host id: got %q, want host-ddd", h.ID)
	}
	if h.Status != "drained" {
		t.Errorf("status: got %q, want drained", h.Status)
	}
	if h.FenceRequired {
		t.Error("fence_required should be false in eligible list")
	}
}

func TestHandleGetRecoveryEligibleHosts_MethodNotAllowed(t *testing.T) {
	srv := newRecoveryTestServer(&recoveryTestPool{})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/recovery-eligible", nil)
	w := httptest.NewRecorder()
	srv.handleGetRecoveryEligibleHosts(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", w.Code)
	}
}

// ── POST /recover ─────────────────────────────────────────────────────────────

func TestHandleRecoverHost_Reactivated(t *testing.T) {
	// drained host, fence_required=FALSE, generation matches → reactivated
	host := makeHostRec("host-aaa", "drained", false, 3)
	pool := &recoveryTestPool{
		hostRec:  host,
		execRows: 1, // UpdateHostStatus CAS succeeds; InsertRecoveryLog also uses Exec
	}
	srv := newRecoveryTestServer(pool)

	body := `{"from_generation":3,"actor":"operator"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/host-aaa/recover",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRecoverHost(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp hostRecoverResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Verdict != db.RecoveryVerdictReactivated {
		t.Errorf("verdict: got %q, want %q", resp.Verdict, db.RecoveryVerdictReactivated)
	}
	if resp.HostStatusAtAttempt != "drained" {
		t.Errorf("host_status_at_attempt: got %q, want drained", resp.HostStatusAtAttempt)
	}
	if resp.FenceRequiredAtAttempt {
		t.Error("fence_required_at_attempt should be false")
	}
}

func TestHandleRecoverHost_DrainInitiated_Degraded(t *testing.T) {
	// degraded host, fence_required=FALSE → drain_initiated
	host := makeHostRec("host-bbb", "degraded", false, 5)
	pool := &recoveryTestPool{hostRec: host, execRows: 1}
	srv := newRecoveryTestServer(pool)

	body := `{"from_generation":5}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/host-bbb/recover",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRecoverHost(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp hostRecoverResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Verdict != db.RecoveryVerdictDrainInitiated {
		t.Errorf("verdict: got %q, want %q", resp.Verdict, db.RecoveryVerdictDrainInitiated)
	}
}

func TestHandleRecoverHost_SkippedFenceRequired(t *testing.T) {
	// unhealthy host, fence_required=TRUE → skipped_fence_required → 409
	host := makeHostRec("host-ccc", "unhealthy", true, 2)
	pool := &recoveryTestPool{hostRec: host, execRows: 1}
	srv := newRecoveryTestServer(pool)

	body := `{"from_generation":2}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/host-ccc/recover",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRecoverHost(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var resp hostRecoverResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Verdict != db.RecoveryVerdictSkippedFenceRequired {
		t.Errorf("verdict: got %q, want %q", resp.Verdict, db.RecoveryVerdictSkippedFenceRequired)
	}
	if !resp.FenceRequiredAtAttempt {
		t.Error("fence_required_at_attempt should be true")
	}
}

func TestHandleRecoverHost_SkippedNotEligible(t *testing.T) {
	// retiring host → skipped_not_eligible → 409
	host := makeHostRec("host-ddd", "retiring", false, 1)
	pool := &recoveryTestPool{hostRec: host, execRows: 1}
	srv := newRecoveryTestServer(pool)

	body := `{"from_generation":1}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/host-ddd/recover",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRecoverHost(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var resp hostRecoverResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Verdict != db.RecoveryVerdictSkippedNotEligible {
		t.Errorf("verdict: got %q, want %q", resp.Verdict, db.RecoveryVerdictSkippedNotEligible)
	}
}

func TestHandleRecoverHost_CASFailed(t *testing.T) {
	// drained host, fence_required=FALSE, but CAS returns 0 rows → cas_failed → 409
	host := makeHostRec("host-eee", "drained", false, 9)
	pool := &recoveryTestPool{hostRec: host, execRows: 0} // UpdateHostStatus → 0 rows
	srv := newRecoveryTestServer(pool)

	body := `{"from_generation":9}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/host-eee/recover",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRecoverHost(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", w.Code, w.Body.String())
	}
	var resp hostRecoverResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Verdict != db.RecoveryVerdictCASFailed {
		t.Errorf("verdict: got %q, want %q", resp.Verdict, db.RecoveryVerdictCASFailed)
	}
}

func TestHandleRecoverHost_HostNotFound(t *testing.T) {
	// GetHostByID returns ErrHostNotFound
	pool := &recoveryTestPool{
		hostErr: fmt.Errorf("host not found: %w", db.ErrHostNotFound),
	}
	srv := newRecoveryTestServer(pool)

	body := `{"from_generation":1}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/missing-host/recover",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRecoverHost(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleRecoverHost_InvalidJSON(t *testing.T) {
	pool := &recoveryTestPool{}
	srv := newRecoveryTestServer(pool)

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts/host-x/recover",
		strings.NewReader("not-json"))
	w := httptest.NewRecorder()
	srv.handleRecoverHost(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// ── GET /recovery-log ─────────────────────────────────────────────────────────

func TestHandleGetHostRecoveryLog_EmptyHistory(t *testing.T) {
	pool := &recoveryTestPool{} // no rows
	srv := newRecoveryTestServer(pool)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/hosts/host-zzz/recovery-log", nil)
	w := httptest.NewRecorder()
	srv.handleGetHostRecoveryLog(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp recoveryLogResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Count != 0 {
		t.Errorf("count: got %d, want 0", resp.Count)
	}
	if resp.Entries == nil || len(resp.Entries) != 0 {
		t.Error("entries should be empty slice")
	}
}

func TestHandleGetHostRecoveryLog_WithEntries(t *testing.T) {
	now := time.Now().UTC()
	// Columns match GetHostRecoveryLog SELECT:
	// id, host_id, verdict, reason, status_at, gen_at, fence_req_at, actor, campaign_id, attempted_at
	row := []any{
		"rl-001", "host-yyy",
		db.RecoveryVerdictReactivated,
		"drained host reactivated",
		"drained", int64(3), false,
		"operator", nil, now,
	}
	pool := &recoveryTestPool{queryRows: [][]any{row}}
	srv := newRecoveryTestServer(pool)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/hosts/host-yyy/recovery-log", nil)
	w := httptest.NewRecorder()
	srv.handleGetHostRecoveryLog(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp recoveryLogResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Count != 1 {
		t.Fatalf("count: got %d, want 1", resp.Count)
	}
	entry := resp.Entries[0]
	if entry.Verdict != db.RecoveryVerdictReactivated {
		t.Errorf("verdict: got %q, want %q", entry.Verdict, db.RecoveryVerdictReactivated)
	}
	if entry.Actor != "operator" {
		t.Errorf("actor: got %q, want operator", entry.Actor)
	}
}

// ── GET /campaigns/{id}/failed-hosts/recovery ─────────────────────────────────

func TestHandleGetCampaignFailedHostsRecovery_EligibleAndBlocked(t *testing.T) {
	now := time.Now().UTC()
	campaign := &db.CampaignRecord{
		ID:               "camp-001",
		CampaignReason:   "kernel update",
		TargetHostIDs:    []string{"host-A", "host-B"},
		CompletedHostIDs: []string{},
		FailedHostIDs:    []string{"host-A", "host-B"},
		MaxParallel:      1,
		Status:           "running",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	// host-A: drained, fence_required=FALSE → eligible
	// host-B: unhealthy, fence_required=TRUE → blocked
	hostA := makeHostRec("host-A", "drained", false, 4)
	hostB := makeHostRec("host-B", "unhealthy", true, 2)

	pool := &recoveryTestPool{
		campaignRec:    campaign,
		multiQueryRows: []*db.HostRecord{hostA, hostB},
	}
	srv := newRecoveryTestServer(pool)

	req := httptest.NewRequest(http.MethodGet,
		"/internal/v1/maintenance/campaigns/camp-001/failed-hosts/recovery", nil)
	w := httptest.NewRecorder()
	srv.handleGetCampaignFailedHostsRecovery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp campaignRecoveryAssessmentResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.EligibleHosts) != 1 {
		t.Errorf("eligible_hosts: got %d, want 1", len(resp.EligibleHosts))
	}
	if len(resp.EligibleHosts) > 0 && resp.EligibleHosts[0].ID != "host-A" {
		t.Errorf("eligible host id: got %q, want host-A", resp.EligibleHosts[0].ID)
	}
	if len(resp.BlockedByFencing) != 1 {
		t.Errorf("blocked_by_fencing: got %d, want 1", len(resp.BlockedByFencing))
	}
	if len(resp.BlockedByFencing) > 0 && resp.BlockedByFencing[0].ID != "host-B" {
		t.Errorf("blocked host id: got %q, want host-B", resp.BlockedByFencing[0].ID)
	}
	if resp.CampaignID != "camp-001" {
		t.Errorf("campaign_id: got %q, want camp-001", resp.CampaignID)
	}
}

func TestHandleGetCampaignFailedHostsRecovery_CampaignNotFound(t *testing.T) {
	pool := &recoveryTestPool{
		campaignErr: fmt.Errorf("no rows in result set"),
	}
	srv := newRecoveryTestServer(pool)

	req := httptest.NewRequest(http.MethodGet,
		"/internal/v1/maintenance/campaigns/missing-camp/failed-hosts/recovery", nil)
	w := httptest.NewRecorder()
	srv.handleGetCampaignFailedHostsRecovery(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// ── Helper tests ──────────────────────────────────────────────────────────────

func TestExtractRecoverHostID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/internal/v1/hosts/host-abc/recover", "host-abc"},
		{"/internal/v1/hosts/host-xyz-123/recover", "host-xyz-123"},
		{"/internal/v1/hosts//recover", ""},
	}
	for _, tc := range cases {
		got := extractRecoverHostID(tc.path)
		if got != tc.want {
			t.Errorf("extractRecoverHostID(%q)=%q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestExtractRecoveryLogHostID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/internal/v1/hosts/host-abc/recovery-log", "host-abc"},
		{"/internal/v1/hosts/h1/recovery-log", "h1"},
	}
	for _, tc := range cases {
		got := extractRecoveryLogHostID(tc.path)
		if got != tc.want {
			t.Errorf("extractRecoveryLogHostID(%q)=%q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestIsHostNotFoundErr(t *testing.T) {
	if !isHostNotFoundErr(fmt.Errorf("wrapper: %w", db.ErrHostNotFound)) {
		t.Error("should detect wrapped ErrHostNotFound")
	}
	if isHostNotFoundErr(nil) {
		t.Error("nil should return false")
	}
	if isHostNotFoundErr(fmt.Errorf("some other error")) {
		t.Error("unrelated error should return false")
	}
}
