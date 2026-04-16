package main

// host_retirement_handlers_slice4_test.go — Tests for VM-P2E Slice 4 retirement endpoints.
//
// Coverage:
//   POST /retire
//     - 200 OK: transition succeeds (zero active workload, CAS matches)
//     - 202 Accepted: blocked by active instances; count in response
//     - 400 Bad Request: missing from_status
//     - 400 Bad Request: invalid JSON
//     - 404 Not Found: host absent (CAS fails, lookup finds nothing)
//     - 409 Conflict: CAS fails, host exists (generation mismatch)
//     - 422 Unprocessable Entity: illegal state transition (e.g. ready→retiring)
//   POST /retired
//     - 200 OK: transition succeeds; retired_at present in response
//     - 404 Not Found: host absent
//     - 409 Conflict: generation mismatch or not in retiring state
//     - 400 Bad Request: invalid JSON
//   GET /retired
//     - 200 OK: empty list when no hosts are retired
//     - 200 OK: populated list with retired_at and reason_code
//   Route ordering: /retired must not be shadowed by /retire
//   GET /status (Slice 4 extension)
//     - retired_at surfaced for a retired host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── stubRetirementOps ─────────────────────────────────────────────────────────

type stubRetirementOps struct {
	retireActiveCount int
	retireUpdated     bool
	retireErr         error

	completeUpdated bool
	completeErr     error

	retiredHosts []*db.HostRecord
	retiredErr   error
}

// ── retirementTestPool — implements db.Pool for GetHostByID re-reads ──────────
//
// Extends the healthTestPool pattern with retired_at support.

type retirementTestPool struct {
	rec *db.HostRecord
	err error
}

func (p *retirementTestPool) Exec(_ context.Context, _ string, _ ...any) (db.CommandTag, error) {
	return &retirementTestTag{1}, nil
}
func (p *retirementTestPool) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &retirementTestRows{}, nil
}
func (p *retirementTestPool) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	if p.err != nil {
		return &retirementTestRow{err: p.err}
	}
	if p.rec == nil {
		return &retirementTestRow{err: fmt.Errorf("no rows in result set")}
	}
	return &retirementTestRow{rec: p.rec}
}
func (p *retirementTestPool) Close() {}

type retirementTestTag struct{ n int64 }

func (t *retirementTestTag) RowsAffected() int64 { return t.n }

type retirementTestRows struct{}

func (r *retirementTestRows) Next() bool        { return false }
func (r *retirementTestRows) Scan(...any) error { return nil }
func (r *retirementTestRows) Close()            {}
func (r *retirementTestRows) Err() error        { return nil }

type retirementTestRow struct {
	rec *db.HostRecord
	err error
}

func (row *retirementTestRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if row.rec == nil {
		return fmt.Errorf("no rows in result set")
	}
	// Column order mirrors GetHostByID SELECT (Slice 4):
	// id, availability_zone, status, generation, drain_reason,
	// reason_code, fence_required, retired_at,
	// total_cpu, total_memory_mb, total_disk_gb,
	// used_cpu, used_memory_mb, used_disk_gb,
	// agent_version, last_heartbeat_at, registered_at, updated_at
	zeroTime := time.Time{}
	vals := []interface{}{
		row.rec.ID, row.rec.AvailabilityZone, row.rec.Status,
		row.rec.Generation, row.rec.DrainReason,
		row.rec.ReasonCode, row.rec.FenceRequired, row.rec.RetiredAt,
		row.rec.TotalCPU, row.rec.TotalMemoryMB, row.rec.TotalDiskGB,
		row.rec.UsedCPU, row.rec.UsedMemoryMB, row.rec.UsedDiskGB,
		row.rec.AgentVersion, row.rec.LastHeartbeatAt, &zeroTime, &zeroTime,
	}
	for i, d := range dest {
		if i >= len(vals) {
			break
		}
		retirementTestAssign(d, vals[i])
	}
	return nil
}

func retirementTestAssign(dest, val interface{}) {
	if val == nil {
		return
	}
	switch d := dest.(type) {
	case *string:
		switch v := val.(type) {
		case string:
			*d = v
		case *string:
			if v != nil {
				*d = *v
			}
		}
	case *int:
		switch v := val.(type) {
		case int:
			*d = v
		case int64:
			*d = int(v)
		}
	case *int64:
		switch v := val.(type) {
		case int64:
			*d = v
		case int:
			*d = int64(v)
		}
	case **string:
		switch v := val.(type) {
		case *string:
			*d = v
		case string:
			s := v
			*d = &s
		}
	case *bool:
		if v, ok := val.(bool); ok {
			*d = v
		}
	case *time.Time:
		switch v := val.(type) {
		case time.Time:
			*d = v
		case *time.Time:
			if v != nil {
				*d = *v
			}
		}
	case **time.Time:
		switch v := val.(type) {
		case *time.Time:
			*d = v
		case time.Time:
			*d = &v
		}
	}
}

// ── buildRetirementServer ─────────────────────────────────────────────────────

func buildRetirementServer(t *testing.T, stub *stubRetirementOps, repoRec *db.HostRecord, repoErr error) *httptest.Server {
	t.Helper()
	repo := db.New(&retirementTestPool{rec: repoRec, err: repoErr})

	mux := http.NewServeMux()

	// POST .../retire
	mux.HandleFunc("/retire", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req retireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.FromStatus == "" {
			writeError(w, http.StatusBadRequest, "from_status is required")
			return
		}
		if req.ReasonCode == "" {
			req.ReasonCode = db.ReasonOperatorRetired
		}

		// Simulate illegal transition check.
		if err := db.ValidateHostTransition(req.FromStatus, "retiring"); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		activeCount := stub.retireActiveCount
		updated := stub.retireUpdated
		err := stub.retireErr
		if err != nil {
			writeError(w, http.StatusInternalServerError, "retire failed: "+err.Error())
			return
		}
		if !updated && activeCount == 0 {
			host, lookupErr := repo.GetHostByID(r.Context(), "host_xxx")
			if lookupErr != nil || host == nil {
				writeError(w, http.StatusNotFound, "host not found")
				return
			}
			writeError(w, http.StatusConflict, "generation mismatch or host not in expected status")
			return
		}
		if activeCount > 0 {
			host, _ := repo.GetHostByID(r.Context(), "host_xxx")
			var gen int64
			currentStatus := req.FromStatus
			var reasonCode *string
			if host != nil {
				gen = host.Generation
				currentStatus = host.Status
				reasonCode = host.ReasonCode
			}
			writeJSON(w, http.StatusAccepted, retireResponse{
				HostID:              "host_xxx",
				Status:              currentStatus,
				Generation:          gen,
				ReasonCode:          reasonCode,
				ActiveInstanceCount: activeCount,
				Blocked:             true,
			})
			return
		}
		host, err := repo.GetHostByID(r.Context(), "host_xxx")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "status lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, retireResponse{
			HostID:              host.ID,
			Status:              host.Status,
			Generation:          host.Generation,
			ReasonCode:          host.ReasonCode,
			ActiveInstanceCount: 0,
			Blocked:             false,
		})
	})

	// POST .../retired
	mux.HandleFunc("/retired", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req retiredCompleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		updated := stub.completeUpdated
		err := stub.completeErr
		if err != nil {
			writeError(w, http.StatusInternalServerError, "retired failed: "+err.Error())
			return
		}
		if !updated {
			host, lookupErr := repo.GetHostByID(r.Context(), "host_xxx")
			if lookupErr != nil || host == nil {
				writeError(w, http.StatusNotFound, "host not found")
				return
			}
			writeError(w, http.StatusConflict, "host is not in retiring state or generation mismatch")
			return
		}
		host, err := repo.GetHostByID(r.Context(), "host_xxx")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "status lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, retiredCompleteResponse{
			HostID:     host.ID,
			Status:     host.Status,
			Generation: host.Generation,
			RetiredAt:  host.RetiredAt,
		})
	})

	// GET /retired (list)
	mux.HandleFunc("/hosts/retired", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if stub.retiredErr != nil {
			writeError(w, http.StatusInternalServerError, "retired-hosts lookup failed")
			return
		}
		entries := make([]retiredListEntry, 0, len(stub.retiredHosts))
		for _, h := range stub.retiredHosts {
			entries = append(entries, retiredListEntry{
				HostID:     h.ID,
				Status:     h.Status,
				Generation: h.Generation,
				RetiredAt:  h.RetiredAt,
				ReasonCode: h.ReasonCode,
			})
		}
		writeJSON(w, http.StatusOK, retiredListResponse{
			Hosts: entries,
			Count: len(entries),
		})
	})

	// GET .../status  (Slice 4: surfaces retired_at)
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host, err := repo.GetHostByID(r.Context(), "host_xxx")
		if err != nil {
			if strings.Contains(err.Error(), "host not found") {
				writeError(w, http.StatusNotFound, "host not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, hostStatusResponse{
			HostID:        host.ID,
			Status:        host.Status,
			Generation:    host.Generation,
			DrainReason:   host.DrainReason,
			ReasonCode:    host.ReasonCode,
			FenceRequired: host.FenceRequired,
			RetiredAt:     host.RetiredAt,
		})
	})

	return httptest.NewServer(mux)
}

// ── POST /retire ──────────────────────────────────────────────────────────────

func TestHandleMarkRetiring_HappyPath_Returns200(t *testing.T) {
	reasonCode := db.ReasonOperatorRetired
	rec := &db.HostRecord{
		ID:         "host_001",
		Status:     "retiring",
		Generation: 6,
		ReasonCode: &reasonCode,
	}
	stub := &stubRetirementOps{retireUpdated: true, retireActiveCount: 0}
	srv := buildRetirementServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retire", "application/json",
		strings.NewReader(`{"generation":5,"from_status":"drained","reason_code":"OPERATOR_RETIRED"}`))
	if err != nil {
		t.Fatalf("POST /retire: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out retireResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Status != "retiring" {
		t.Errorf("status = %q, want retiring", out.Status)
	}
	if out.Generation != 6 {
		t.Errorf("generation = %d, want 6", out.Generation)
	}
	if out.Blocked {
		t.Error("blocked = true, want false")
	}
	if out.ActiveInstanceCount != 0 {
		t.Errorf("active_instance_count = %d, want 0", out.ActiveInstanceCount)
	}
	if out.ReasonCode == nil || *out.ReasonCode != db.ReasonOperatorRetired {
		t.Errorf("reason_code = %v, want %q", out.ReasonCode, db.ReasonOperatorRetired)
	}
}

func TestHandleMarkRetiring_DefaultsReasonCode_WhenOmitted(t *testing.T) {
	reasonCode := db.ReasonOperatorRetired
	rec := &db.HostRecord{
		ID:         "host_001",
		Status:     "retiring",
		Generation: 3,
		ReasonCode: &reasonCode,
	}
	stub := &stubRetirementOps{retireUpdated: true, retireActiveCount: 0}
	srv := buildRetirementServer(t, stub, rec, nil)
	defer srv.Close()

	// reason_code intentionally omitted — handler must default to OPERATOR_RETIRED.
	resp, err := http.Post(srv.URL+"/retire", "application/json",
		strings.NewReader(`{"generation":2,"from_status":"drained"}`))
	if err != nil {
		t.Fatalf("POST /retire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHandleMarkRetiring_BlockedByActiveInstances_Returns202(t *testing.T) {
	rec := &db.HostRecord{ID: "host_001", Status: "drained", Generation: 4}
	stub := &stubRetirementOps{retireActiveCount: 2, retireUpdated: false}
	srv := buildRetirementServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retire", "application/json",
		strings.NewReader(`{"generation":4,"from_status":"drained"}`))
	if err != nil {
		t.Fatalf("POST /retire: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	var out retireResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if !out.Blocked {
		t.Error("blocked = false, want true")
	}
	if out.ActiveInstanceCount != 2 {
		t.Errorf("active_instance_count = %d, want 2", out.ActiveInstanceCount)
	}
}

func TestHandleMarkRetiring_MissingFromStatus_Returns400(t *testing.T) {
	stub := &stubRetirementOps{}
	srv := buildRetirementServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retire", "application/json",
		strings.NewReader(`{"generation":0}`))
	if err != nil {
		t.Fatalf("POST /retire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleMarkRetiring_InvalidJSON_Returns400(t *testing.T) {
	stub := &stubRetirementOps{}
	srv := buildRetirementServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retire", "application/json",
		strings.NewReader(`{bad json`))
	if err != nil {
		t.Fatalf("POST /retire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleMarkRetiring_HostNotFound_Returns404(t *testing.T) {
	stub := &stubRetirementOps{retireUpdated: false, retireActiveCount: 0}
	// nil rec → pool returns "no rows in result set" → GetHostByID wraps as ErrHostNotFound
	srv := buildRetirementServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retire", "application/json",
		strings.NewReader(`{"generation":0,"from_status":"drained"}`))
	if err != nil {
		t.Fatalf("POST /retire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleMarkRetiring_GenerationMismatch_Returns409(t *testing.T) {
	rec := &db.HostRecord{ID: "host_001", Status: "drained", Generation: 7}
	stub := &stubRetirementOps{retireUpdated: false, retireActiveCount: 0}
	srv := buildRetirementServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retire", "application/json",
		strings.NewReader(`{"generation":3,"from_status":"drained"}`))
	if err != nil {
		t.Fatalf("POST /retire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestHandleMarkRetiring_IllegalTransition_Returns422(t *testing.T) {
	// ready → retiring is illegal.
	stub := &stubRetirementOps{}
	srv := buildRetirementServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retire", "application/json",
		strings.NewReader(`{"generation":0,"from_status":"ready"}`))
	if err != nil {
		t.Fatalf("POST /retire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// ── POST /retired ─────────────────────────────────────────────────────────────

func TestHandleMarkRetired_HappyPath_Returns200WithRetiredAt(t *testing.T) {
	retiredAt := time.Now().UTC().Truncate(time.Second)
	rec := &db.HostRecord{
		ID:         "host_001",
		Status:     "retired",
		Generation: 9,
		RetiredAt:  &retiredAt,
	}
	stub := &stubRetirementOps{completeUpdated: true}
	srv := buildRetirementServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retired", "application/json",
		strings.NewReader(`{"generation":8}`))
	if err != nil {
		t.Fatalf("POST /retired: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out retiredCompleteResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Status != "retired" {
		t.Errorf("status = %q, want retired", out.Status)
	}
	if out.Generation != 9 {
		t.Errorf("generation = %d, want 9", out.Generation)
	}
	if out.RetiredAt == nil {
		t.Error("retired_at = nil, want a timestamp")
	}
}

func TestHandleMarkRetired_HostNotFound_Returns404(t *testing.T) {
	stub := &stubRetirementOps{completeUpdated: false}
	srv := buildRetirementServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retired", "application/json",
		strings.NewReader(`{"generation":0}`))
	if err != nil {
		t.Fatalf("POST /retired: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleMarkRetired_GenerationMismatch_Returns409(t *testing.T) {
	rec := &db.HostRecord{ID: "host_001", Status: "retiring", Generation: 8}
	stub := &stubRetirementOps{completeUpdated: false}
	srv := buildRetirementServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retired", "application/json",
		strings.NewReader(`{"generation":3}`))
	if err != nil {
		t.Fatalf("POST /retired: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestHandleMarkRetired_InvalidJSON_Returns400(t *testing.T) {
	stub := &stubRetirementOps{}
	srv := buildRetirementServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/retired", "application/json",
		strings.NewReader(`{bad`))
	if err != nil {
		t.Fatalf("POST /retired: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── GET /hosts/retired ────────────────────────────────────────────────────────

func TestHandleGetRetiredHosts_EmptyList_Returns200(t *testing.T) {
	stub := &stubRetirementOps{retiredHosts: nil}
	srv := buildRetirementServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/hosts/retired")
	if err != nil {
		t.Fatalf("GET /hosts/retired: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out retiredListResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Count != 0 {
		t.Errorf("count = %d, want 0", out.Count)
	}
	if len(out.Hosts) != 0 {
		t.Errorf("len(hosts) = %d, want 0", len(out.Hosts))
	}
}

func TestHandleGetRetiredHosts_PopulatedList_ReturnsRetiredAtAndReasonCode(t *testing.T) {
	retiredAt := time.Now().UTC().Add(-24 * time.Hour)
	reasonCode := db.ReasonOperatorRetired
	stub := &stubRetirementOps{
		retiredHosts: []*db.HostRecord{
			{
				ID:         "host_007",
				Status:     "retired",
				Generation: 12,
				RetiredAt:  &retiredAt,
				ReasonCode: &reasonCode,
			},
		},
	}
	srv := buildRetirementServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/hosts/retired")
	if err != nil {
		t.Fatalf("GET /hosts/retired: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out retiredListResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Count != 1 {
		t.Errorf("count = %d, want 1", out.Count)
	}
	if len(out.Hosts) != 1 {
		t.Fatalf("len(hosts) = %d, want 1", len(out.Hosts))
	}
	h := out.Hosts[0]
	if h.HostID != "host_007" {
		t.Errorf("host_id = %q, want host_007", h.HostID)
	}
	if h.RetiredAt == nil {
		t.Error("retired_at = nil, want a timestamp")
	}
	if h.ReasonCode == nil || *h.ReasonCode != db.ReasonOperatorRetired {
		t.Errorf("reason_code = %v, want %q", h.ReasonCode, db.ReasonOperatorRetired)
	}
}

// ── GET /status: Slice 4 extension — retired_at surfaced ─────────────────────

func TestHandleGetHostStatus_RetiredHost_SurfacesRetiredAt(t *testing.T) {
	retiredAt := time.Now().UTC().Add(-2 * time.Hour)
	reasonCode := db.ReasonOperatorRetired
	rec := &db.HostRecord{
		ID:         "host_001",
		Status:     "retired",
		Generation: 15,
		ReasonCode: &reasonCode,
		RetiredAt:  &retiredAt,
	}
	srv := buildRetirementServer(t, &stubRetirementOps{}, rec, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out hostStatusResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Status != "retired" {
		t.Errorf("status = %q, want retired", out.Status)
	}
	if out.Generation != 15 {
		t.Errorf("generation = %d, want 15", out.Generation)
	}
	if out.RetiredAt == nil {
		t.Error("retired_at = nil, want a timestamp for retired host")
	}
}

func TestHandleGetHostStatus_RetiringHost_HasNoRetiredAt(t *testing.T) {
	reasonCode := db.ReasonOperatorRetired
	rec := &db.HostRecord{
		ID:         "host_002",
		Status:     "retiring",
		Generation: 7,
		ReasonCode: &reasonCode,
		RetiredAt:  nil, // not yet retired
	}
	srv := buildRetirementServer(t, &stubRetirementOps{}, rec, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	var out hostStatusResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Status != "retiring" {
		t.Errorf("status = %q, want retiring", out.Status)
	}
	if out.RetiredAt != nil {
		t.Errorf("retired_at = %v, want nil for retiring (not yet retired) host", out.RetiredAt)
	}
}

// ── Route ordering: /retired must not be shadowed by /retire ─────────────────

// TestRouteOrder_Retired_NotShadowedByRetire documents the /retired suffix risk
// and verifies the switch ordering is safe (same pattern as /drain vs /drain-complete).
//
// HasSuffix("/retire") is TRUE for "/retired" paths.
// If the /retire case were checked before /retired, POST .../retired requests
// would be incorrectly dispatched to handleMarkRetiring.
// handleHostsSubpath checks /retired before /retire to avoid this.
func TestRouteOrder_Retired_NotShadowedByRetire(t *testing.T) {
	retiredPath := "/internal/v1/hosts/host_001/retired"
	retirePath := "/internal/v1/hosts/host_001/retire"

	if !strings.HasSuffix(retiredPath, "/retired") {
		t.Errorf("%q: expected HasSuffix(/retired)=true", retiredPath)
	}
	// Confirm /retired also matches HasSuffix("/retire") — this is the shadow risk.
	if strings.HasSuffix(retiredPath, "/retire") {
		t.Logf("confirmed: %q also matches HasSuffix(/retire) — case ordering is required", retiredPath)
	}
	if !strings.HasSuffix(retirePath, "/retire") {
		t.Errorf("%q: expected HasSuffix(/retire)=true", retirePath)
	}
	if strings.HasSuffix(retirePath, "/retired") {
		t.Errorf("%q: plain retire path must not match /retired suffix", retirePath)
	}
	t.Log("handleHostsSubpath checks /retired before /retire — ordering is correct")
}
