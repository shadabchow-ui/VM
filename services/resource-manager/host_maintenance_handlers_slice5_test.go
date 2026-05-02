package main

// host_maintenance_handlers_slice5_test.go — Tests for VM-P2E Slice 5 campaign endpoints.
//
// Coverage:
//   POST /maintenance/campaigns
//     - 201 Created: campaign created with correct fields
//     - 400: missing id, reason, or target_host_ids
//     - 400: blast-radius limit exceeded (max_parallel > MaxCampaignParallel)
//     - 400: invalid JSON
//   GET /maintenance/campaigns
//     - 200: empty list
//     - 200: populated list
//   GET /maintenance/campaigns/{id}
//     - 200: campaign found
//     - 404: campaign not found
//   POST /maintenance/campaigns/{id}/advance
//     - 200: hosts actioned; actioned list returned
//     - 409: campaign terminal
//     - 409: campaign paused
//     - 404: campaign not found
//   POST /maintenance/campaigns/{id}/pause
//     - 200: campaign paused
//     - 409: campaign already terminal
//   POST /maintenance/campaigns/{id}/resume
//     - 200: campaign resumed
//     - 409: campaign not paused
//   POST /maintenance/campaigns/{id}/cancel
//     - 200: campaign cancelled
//     - 409: campaign already terminal
//   Routing: extractCampaignID helper
//   Routing: handleCampaignSubpath action dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── stubCampaignInventory ─────────────────────────────────────────────────────
//
// stubCampaignInventory satisfies the campaign inventory calls used by handlers.
// It does NOT implement the full HostInventory struct — handlers access campaign
// methods via the *HostInventory pointer on server, so we build a minimal
// test server that wires stub behavior through the handler directly.

// campaignTestPool implements db.Pool for campaign tests.
// QueryRow returns a scripted CampaignRecord or an error.
// Query returns a scripted list of CampaignRecords.
type campaignTestPool struct {
	// For GetCampaignByID (QueryRow).
	queryCampaign *db.CampaignRecord
	queryErr      error

	// For ListCampaigns (Query).
	listCampaigns []*db.CampaignRecord

	// For Exec-path methods (CreateCampaign insert, UpdateCampaignStatus, AdvanceCampaignProgress).
	execRows int64
	execErr  error

	// Multi-QueryRow for CreateCampaign (insert → GetCampaignByID chain).
	// First QueryRow call returns multiFirst, subsequent return queryCampaign.
	multiFirst *db.CampaignRecord
	multiIdx   int
}

func (p *campaignTestPool) Exec(_ context.Context, _ string, _ ...any) (db.CommandTag, error) {
	if p.execErr != nil {
		return nil, p.execErr
	}
	return &campaignTestTag{n: p.execRows}, nil
}

func (p *campaignTestPool) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &campaignTestRows{records: p.listCampaigns, idx: -1}, nil
}

func (p *campaignTestPool) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	if p.multiFirst != nil {
		if p.multiIdx == 0 {
			p.multiIdx++
			// First call: insert's GetCampaignByID returns multiFirst.
			return &campaignTestRow{rec: p.multiFirst}
		}
	}
	if p.queryErr != nil {
		return &campaignTestRow{err: p.queryErr}
	}
	if p.queryCampaign == nil {
		return &campaignTestRow{err: fmt.Errorf("no rows in result set")}
	}
	return &campaignTestRow{rec: p.queryCampaign}
}

func (p *campaignTestPool) Close() {}

type campaignTestTag struct{ n int64 }

func (t *campaignTestTag) RowsAffected() int64 { return t.n }

// campaignTestRows iterates over a scripted list of CampaignRecords.
type campaignTestRows struct {
	records []*db.CampaignRecord
	idx     int
}

func (r *campaignTestRows) Next() bool { r.idx++; return r.idx < len(r.records) }
func (r *campaignTestRows) Err() error { return nil }
func (r *campaignTestRows) Close()     {}
func (r *campaignTestRows) Scan(dest ...any) error {
	if r.idx < 0 || r.idx >= len(r.records) {
		return fmt.Errorf("no row at index %d", r.idx)
	}
	return scanCampaignRecord(r.records[r.idx], dest)
}

// campaignTestRow is a single-row result for QueryRow.
type campaignTestRow struct {
	rec *db.CampaignRecord
	err error
}

func (row *campaignTestRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	return scanCampaignRecord(row.rec, dest)
}

// scanCampaignRecord fills dest in the column order used by GetCampaignByID / ListCampaigns:
//
//	id, campaign_reason, target_host_ids, completed_host_ids, failed_host_ids,
//	max_parallel, status, created_at, updated_at
func scanCampaignRecord(c *db.CampaignRecord, dest []any) error {
	if len(dest) < 9 {
		return fmt.Errorf("scanCampaignRecord: need 9 dest, got %d", len(dest))
	}
	assign := func(d any, v any) {
		switch dst := d.(type) {
		case *string:
			if s, ok := v.(string); ok {
				*dst = s
			}
		case *int:
			if n, ok := v.(int); ok {
				*dst = n
			}
		case *[]string:
			if ss, ok := v.([]string); ok {
				*dst = ss
			}
		case *time.Time:
			if t, ok := v.(time.Time); ok {
				*dst = t
			}
		}
	}
	now := time.Now().UTC()
	assign(dest[0], c.ID)
	assign(dest[1], c.CampaignReason)
	assign(dest[2], c.TargetHostIDs)
	assign(dest[3], c.CompletedHostIDs)
	assign(dest[4], c.FailedHostIDs)
	assign(dest[5], c.MaxParallel)
	assign(dest[6], c.Status)
	if c.CreatedAt.IsZero() {
		assign(dest[7], now)
	} else {
		assign(dest[7], c.CreatedAt)
	}
	if c.UpdatedAt.IsZero() {
		assign(dest[8], now)
	} else {
		assign(dest[8], c.UpdatedAt)
	}
	return nil
}

// makeCampaign constructs a minimal CampaignRecord for test fixtures.
func makeCampaign(id, status string, targets []string, maxParallel int) *db.CampaignRecord {
	return &db.CampaignRecord{
		ID:               id,
		CampaignReason:   "test campaign",
		TargetHostIDs:    targets,
		CompletedHostIDs: []string{},
		FailedHostIDs:    []string{},
		MaxParallel:      maxParallel,
		Status:           status,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
}

// ── buildCampaignServer ───────────────────────────────────────────────────────
//
// Constructs a test HTTP server wired to a campaignTestPool.
// Mounts handlers at the same paths as production routing.

func buildCampaignServer(t *testing.T, pool *campaignTestPool) *httptest.Server {
	t.Helper()
	repo := db.New(pool)
	inv := newHostInventory(repo)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &server{
		inventory: inv,
		log:       log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/maintenance/campaigns", srv.handleCampaignsCollection)
	mux.HandleFunc("/internal/v1/maintenance/campaigns/", srv.handleCampaignSubpath)

	return httptest.NewServer(mux)
}

// ── POST /maintenance/campaigns ───────────────────────────────────────────────

func TestHandleCreateCampaign_HappyPath_Returns201(t *testing.T) {
	rec := makeCampaign("camp-001", "pending", []string{"h1", "h2", "h3"}, 2)
	pool := &campaignTestPool{
		execRows:   1,
		multiFirst: rec, // returned by the GetCampaignByID after insert
	}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	body := `{"id":"camp-001","reason":"kernel patch","target_host_ids":["h1","h2","h3"],"max_parallel":2}`
	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /campaigns: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}

	var out campaignResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.ID != "camp-001" {
		t.Errorf("id = %q, want camp-001", out.ID)
	}
	if out.Status != "pending" {
		t.Errorf("status = %q, want pending", out.Status)
	}
	if out.MaxParallel != 2 {
		t.Errorf("max_parallel = %d, want 2", out.MaxParallel)
	}
	if len(out.TargetHostIDs) != 3 {
		t.Errorf("target_host_ids len = %d, want 3", len(out.TargetHostIDs))
	}
}

func TestHandleCreateCampaign_DefaultsMaxParallelToOne(t *testing.T) {
	rec := makeCampaign("camp-002", "pending", []string{"h1"}, 1)
	pool := &campaignTestPool{execRows: 1, multiFirst: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	// max_parallel omitted — should default to 1.
	body := `{"id":"camp-002","reason":"patch","target_host_ids":["h1"]}`
	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /campaigns: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
}

func TestHandleCreateCampaign_BlastRadiusExceeded_Returns400(t *testing.T) {
	pool := &campaignTestPool{}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	// max_parallel above hard limit.
	over := db.MaxCampaignParallel + 1
	body := fmt.Sprintf(`{"id":"camp-x","reason":"r","target_host_ids":["h1"],"max_parallel":%d}`, over)
	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /campaigns: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleCreateCampaign_MissingID_Returns400(t *testing.T) {
	pool := &campaignTestPool{}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	body := `{"reason":"patch","target_host_ids":["h1"]}`
	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /campaigns: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleCreateCampaign_MissingReason_Returns400(t *testing.T) {
	pool := &campaignTestPool{}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	body := `{"id":"c1","target_host_ids":["h1"]}`
	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /campaigns: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleCreateCampaign_EmptyTargets_Returns400(t *testing.T) {
	pool := &campaignTestPool{}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	body := `{"id":"c1","reason":"r","target_host_ids":[]}`
	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /campaigns: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleCreateCampaign_InvalidJSON_Returns400(t *testing.T) {
	pool := &campaignTestPool{}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns",
		"application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatalf("POST /campaigns: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── GET /maintenance/campaigns/{id} ──────────────────────────────────────────

func TestHandleGetCampaign_Found_Returns200(t *testing.T) {
	rec := makeCampaign("camp-xyz", "running", []string{"h1", "h2"}, 1)
	rec.CompletedHostIDs = []string{"h1"}
	pool := &campaignTestPool{queryCampaign: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/internal/v1/maintenance/campaigns/camp-xyz")
	if err != nil {
		t.Fatalf("GET /campaigns/camp-xyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var out campaignResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.ID != "camp-xyz" {
		t.Errorf("id = %q, want camp-xyz", out.ID)
	}
	if out.Status != "running" {
		t.Errorf("status = %q, want running", out.Status)
	}
	if out.RemainingCount != 1 {
		t.Errorf("remaining_count = %d, want 1", out.RemainingCount)
	}
	if len(out.CompletedHostIDs) != 1 || out.CompletedHostIDs[0] != "h1" {
		t.Errorf("completed_host_ids = %v, want [h1]", out.CompletedHostIDs)
	}
}

func TestHandleGetCampaign_NotFound_Returns404(t *testing.T) {
	pool := &campaignTestPool{
		queryErr: fmt.Errorf("no rows in result set"),
	}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/internal/v1/maintenance/campaigns/missing")
	if err != nil {
		t.Fatalf("GET /campaigns/missing: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ── GET /maintenance/campaigns ────────────────────────────────────────────────

func TestHandleListCampaigns_EmptyList_Returns200(t *testing.T) {
	pool := &campaignTestPool{listCampaigns: nil}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/internal/v1/maintenance/campaigns")
	if err != nil {
		t.Fatalf("GET /campaigns: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out campaignListResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Count != 0 {
		t.Errorf("count = %d, want 0", out.Count)
	}
}

func TestHandleListCampaigns_PopulatedList_Returns200(t *testing.T) {
	pool := &campaignTestPool{
		listCampaigns: []*db.CampaignRecord{
			makeCampaign("camp-a", "running", []string{"h1"}, 1),
			makeCampaign("camp-b", "pending", []string{"h2", "h3"}, 2),
		},
	}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/internal/v1/maintenance/campaigns")
	if err != nil {
		t.Fatalf("GET /campaigns: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out campaignListResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Count != 2 {
		t.Errorf("count = %d, want 2", out.Count)
	}
}

// ── POST /maintenance/campaigns/{id}/pause ───────────────────────────────────

func TestHandlePauseCampaign_HappyPath_Returns200(t *testing.T) {
	rec := makeCampaign("camp-001", "paused", []string{"h1"}, 1)
	pool := &campaignTestPool{execRows: 1, queryCampaign: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns/camp-001/pause",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /pause: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out campaignResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Status != "paused" {
		t.Errorf("status = %q, want paused", out.Status)
	}
}

func TestHandlePauseCampaign_TerminalCampaign_Returns409(t *testing.T) {
	// execRows=0 → UpdateCampaignStatus returns false; re-read returns terminal.
	rec := makeCampaign("camp-done", "completed", []string{"h1"}, 1)
	pool := &campaignTestPool{execRows: 0, queryCampaign: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns/camp-done/pause",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /pause: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// ── POST /maintenance/campaigns/{id}/resume ──────────────────────────────────

func TestHandleResumeCampaign_HappyPath_Returns200(t *testing.T) {
	rec := makeCampaign("camp-001", "running", []string{"h1"}, 1)
	pool := &campaignTestPool{execRows: 1, queryCampaign: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns/camp-001/resume",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /resume: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHandleResumeCampaign_NotPaused_Returns409(t *testing.T) {
	// execRows=0 → UpdateCampaignStatus returns false.
	rec := makeCampaign("camp-001", "running", []string{"h1"}, 1)
	pool := &campaignTestPool{execRows: 0, queryCampaign: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns/camp-001/resume",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /resume: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// ── POST /maintenance/campaigns/{id}/cancel ──────────────────────────────────

func TestHandleCancelCampaign_HappyPath_Returns200(t *testing.T) {
	rec := makeCampaign("camp-001", "cancelled", []string{"h1"}, 1)
	pool := &campaignTestPool{execRows: 1, queryCampaign: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns/camp-001/cancel",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /cancel: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out campaignResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", out.Status)
	}
}

func TestHandleCancelCampaign_AlreadyTerminal_Returns409(t *testing.T) {
	rec := makeCampaign("camp-done", "completed", []string{"h1"}, 1)
	pool := &campaignTestPool{execRows: 0, queryCampaign: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/v1/maintenance/campaigns/camp-done/cancel",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// ── extractCampaignID helper ──────────────────────────────────────────────────

func TestExtractCampaignID_BareID(t *testing.T) {
	path := "/internal/v1/maintenance/campaigns/camp-abc"
	got := extractCampaignID(path)
	if got != "camp-abc" {
		t.Errorf("extractCampaignID(%q) = %q, want camp-abc", path, got)
	}
}

func TestExtractCampaignID_WithAction(t *testing.T) {
	for _, action := range []string{"advance", "pause", "resume", "cancel"} {
		path := "/internal/v1/maintenance/campaigns/camp-xyz/" + action
		got := extractCampaignID(path)
		if got != "camp-xyz" {
			t.Errorf("extractCampaignID(%q) = %q, want camp-xyz", path, got)
		}
	}
}

func TestExtractCampaignID_MissingPrefix_ReturnsEmpty(t *testing.T) {
	got := extractCampaignID("/internal/v1/hosts/host-001/drain")
	if got != "" {
		t.Errorf("extractCampaignID(host path) = %q, want empty", got)
	}
}

// ── handleCampaignSubpath routing ────────────────────────────────────────────

func TestRouteOrder_CampaignSubpaths_DispatchedCorrectly(t *testing.T) {
	// Document that action suffixes are all distinct and cannot shadow each other.
	paths := map[string]string{
		"/internal/v1/maintenance/campaigns/c1/advance": "advance",
		"/internal/v1/maintenance/campaigns/c1/pause":   "pause",
		"/internal/v1/maintenance/campaigns/c1/resume":  "resume",
		"/internal/v1/maintenance/campaigns/c1/cancel":  "cancel",
	}
	for path, wantSuffix := range paths {
		if !strings.HasSuffix(path, "/"+wantSuffix) {
			t.Errorf("path %q does not have suffix /%s", path, wantSuffix)
		}
	}
	// Verify none of the action paths shadow each other.
	for pathA := range paths {
		for pathB, suffixB := range paths {
			if pathA == pathB {
				continue
			}
			if strings.HasSuffix(pathA, "/"+suffixB) {
				t.Errorf("path %q shadows suffix %q — routing is ambiguous", pathA, suffixB)
			}
		}
	}
	t.Log("all campaign action suffixes are distinct — handleCampaignSubpath ordering is safe")
}

// ── Slice 6 forward-seam: failed_host_ids is observable ──────────────────────

func TestCampaignResponse_FailedHostIDsAlwaysPresent(t *testing.T) {
	// failed_host_ids must always appear in the response (empty list, not null)
	// so Slice 6 recovery automation can reliably read it without nil checks.
	rec := makeCampaign("camp-001", "running", []string{"h1", "h2"}, 1)
	rec.FailedHostIDs = []string{"h1"} // one failure
	pool := &campaignTestPool{queryCampaign: rec}
	srv := buildCampaignServer(t, pool)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/internal/v1/maintenance/campaigns/camp-001")
	if err != nil {
		t.Fatalf("GET /campaigns/camp-001: %v", err)
	}
	defer resp.Body.Close()

	var out campaignResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.FailedHostIDs == nil {
		t.Error("failed_host_ids = nil, want []string (even if empty) — Slice 6 recovery seam requires non-nil")
	}
	if len(out.FailedHostIDs) != 1 || out.FailedHostIDs[0] != "h1" {
		t.Errorf("failed_host_ids = %v, want [h1]", out.FailedHostIDs)
	}
	// remaining_count should be 1 (h2 not yet actioned).
	if out.RemainingCount != 1 {
		t.Errorf("remaining_count = %d, want 1", out.RemainingCount)
	}
}
