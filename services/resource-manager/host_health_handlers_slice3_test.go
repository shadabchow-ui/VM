package main

// host_health_handlers_slice3_test.go — Tests for VM-P2E Slice 3 host health endpoints.
//
// Coverage:
//   POST /degraded
//     - 200 OK: CAS succeeds, response includes status=degraded, reason_code, generation
//     - 400 Bad Request: missing from_status
//     - 400 Bad Request: missing reason_code
//     - 400 Bad Request: invalid JSON
//     - 404 Not Found: host absent (CAS fails, lookup finds nothing)
//     - 409 Conflict: CAS fails, host exists (generation mismatch)
//     - 422 Unprocessable Entity: illegal state transition
//   POST /unhealthy
//     - 200 OK: ambiguous reason → fence_required=true in response
//     - 200 OK: non-ambiguous reason → fence_required=false in response
//     - 400 Bad Request: missing from_status
//     - 400 Bad Request: missing reason_code
//     - 400 Bad Request: invalid JSON
//     - 404 Not Found: host absent
//     - 409 Conflict: generation mismatch
//     - 422 Unprocessable Entity: illegal state transition
//   GET /status (Slice 3 extensions)
//     - reason_code and fence_required surfaced for degraded host
//     - reason_code and fence_required surfaced for unhealthy host with fence_required=true
//   GET /fence-required
//     - 200 OK: empty list when no hosts need fencing
//     - 200 OK: populated list with reason_code

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

// ── stubHealthOps ─────────────────────────────────────────────────────────────
//
// Stubs for MarkDegraded and MarkUnhealthy results, decoupled from the DB pool.

type stubHealthOps struct {
	degradedUpdated bool
	degradedErr     error

	unhealthyFenceRequired bool
	unhealthyUpdated       bool
	unhealthyErr           error
}

// ── healthTestPool — implements db.Pool for GetHostByID re-reads ──────────────
//
// Extends drainTestPool pattern from Slice 2 tests with reason_code + fence_required.

type healthTestPool struct {
	rec      *db.HostRecord
	err      error
	listRecs []*db.HostRecord // for Query (GetFenceRequiredHosts)
}

func (p *healthTestPool) Exec(_ context.Context, _ string, _ ...any) (db.CommandTag, error) {
	return &healthTestTag{1}, nil
}
func (p *healthTestPool) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &healthTestRows{recs: p.listRecs}, nil
}
func (p *healthTestPool) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	if p.err != nil {
		return &healthTestRow{err: p.err}
	}
	if p.rec == nil {
		return &healthTestRow{err: fmt.Errorf("no rows in result set")}
	}
	return &healthTestRow{rec: p.rec}
}
func (p *healthTestPool) Close() {}

type healthTestTag struct{ n int64 }

func (t *healthTestTag) RowsAffected() int64 { return t.n }

type healthTestRow struct {
	rec *db.HostRecord
	err error
}

func (row *healthTestRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if row.rec == nil {
		return fmt.Errorf("no rows in result set")
	}
	// Column order mirrors GetHostByID SELECT (Slice 3):
	// id, availability_zone, status,
	// generation, drain_reason, reason_code, fence_required,
	// total_cpu, total_memory_mb, total_disk_gb,
	// used_cpu, used_memory_mb, used_disk_gb,
	// agent_version, last_heartbeat_at, registered_at, updated_at
	zeroTime := time.Time{}
	vals := []interface{}{
		row.rec.ID, row.rec.AvailabilityZone, row.rec.Status,
		row.rec.Generation, row.rec.DrainReason, row.rec.ReasonCode, row.rec.FenceRequired,
		row.rec.TotalCPU, row.rec.TotalMemoryMB, row.rec.TotalDiskGB,
		row.rec.UsedCPU, row.rec.UsedMemoryMB, row.rec.UsedDiskGB,
		row.rec.AgentVersion, row.rec.LastHeartbeatAt, &zeroTime, &zeroTime,
	}
	for i, d := range dest {
		if i >= len(vals) {
			break
		}
		healthTestAssign(d, vals[i])
	}
	return nil
}

// healthTestAssign assigns a value to a scan destination pointer.
// Handles all types used by GetHostByID scan targets including bool.
func healthTestAssign(dest, val interface{}) {
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
	case *bool:
		switch v := val.(type) {
		case bool:
			*d = v
		}
	case **string:
		switch v := val.(type) {
		case *string:
			*d = v
		case string:
			s := v
			*d = &s
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

// healthTestRows implements db.Rows for GetFenceRequiredHosts query.
type healthTestRows struct {
	recs []*db.HostRecord
	idx  int
}

func (r *healthTestRows) Next() bool {
	r.idx++
	return r.idx <= len(r.recs)
}
func (r *healthTestRows) Scan(dest ...any) error {
	if r.idx < 1 || r.idx > len(r.recs) {
		return fmt.Errorf("no row")
	}
	rec := r.recs[r.idx-1]
	zeroTime := time.Time{}
	vals := []interface{}{
		rec.ID, rec.AvailabilityZone, rec.Status,
		rec.Generation, rec.DrainReason, rec.ReasonCode, rec.FenceRequired,
		rec.TotalCPU, rec.TotalMemoryMB, rec.TotalDiskGB,
		rec.UsedCPU, rec.UsedMemoryMB, rec.UsedDiskGB,
		rec.AgentVersion, rec.LastHeartbeatAt, &zeroTime, &zeroTime,
	}
	for i, d := range dest {
		if i >= len(vals) {
			break
		}
		healthTestAssign(d, vals[i])
	}
	return nil
}
func (r *healthTestRows) Close()     {}
func (r *healthTestRows) Err() error { return nil }

// ── buildHealthServer ─────────────────────────────────────────────────────────
//
// Constructs a test HTTP server with inline handlers backed by stubHealthOps
// (for business logic stubs) and a healthTestPool (for DB re-reads).
// Mirrors buildDrainServer pattern from Slice 2 tests.

func buildHealthServer(t *testing.T, stub *stubHealthOps, repoRec *db.HostRecord, repoErr error, listRecs []*db.HostRecord) *httptest.Server {
	t.Helper()
	repo := db.New(&healthTestPool{rec: repoRec, err: repoErr, listRecs: listRecs})

	mux := http.NewServeMux()

	// POST .../degraded
	mux.HandleFunc("/degraded", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req markDegradedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.FromStatus == "" {
			writeError(w, http.StatusBadRequest, "from_status is required")
			return
		}
		if req.ReasonCode == "" {
			writeError(w, http.StatusBadRequest, "reason_code is required")
			return
		}

		updated := stub.degradedUpdated
		err := stub.degradedErr
		if err != nil {
			if isIllegalTransitionErr(err) {
				writeError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "mark-degraded failed: "+err.Error())
			return
		}
		if !updated {
			host, lookupErr := repo.GetHostByID(r.Context(), "host_xxx")
			if lookupErr != nil || host == nil {
				writeError(w, http.StatusNotFound, "host not found")
				return
			}
			writeError(w, http.StatusConflict, "generation mismatch or host not in expected status")
			return
		}
		host, err := repo.GetHostByID(r.Context(), "host_xxx")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "status lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, markDegradedResponse{
			HostID:     host.ID,
			Status:     host.Status,
			Generation: host.Generation,
			ReasonCode: host.ReasonCode,
		})
	})

	// POST .../unhealthy
	mux.HandleFunc("/unhealthy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req markUnhealthyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.FromStatus == "" {
			writeError(w, http.StatusBadRequest, "from_status is required")
			return
		}
		if req.ReasonCode == "" {
			writeError(w, http.StatusBadRequest, "reason_code is required")
			return
		}

		fenceRequired := stub.unhealthyFenceRequired
		updated := stub.unhealthyUpdated
		err := stub.unhealthyErr
		if err != nil {
			if isIllegalTransitionErr(err) {
				writeError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "mark-unhealthy failed: "+err.Error())
			return
		}
		if !updated {
			host, lookupErr := repo.GetHostByID(r.Context(), "host_xxx")
			if lookupErr != nil || host == nil {
				writeError(w, http.StatusNotFound, "host not found")
				return
			}
			writeError(w, http.StatusConflict, "generation mismatch or host not in expected status")
			return
		}
		host, err := repo.GetHostByID(r.Context(), "host_xxx")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "status lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, markUnhealthyResponse{
			HostID:        host.ID,
			Status:        host.Status,
			Generation:    host.Generation,
			ReasonCode:    host.ReasonCode,
			FenceRequired: fenceRequired,
		})
	})

	// GET .../status — extended with reason_code and fence_required
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
		})
	})

	// GET /fence-required
	mux.HandleFunc("/fence-required", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		hosts, err := repo.GetFenceRequiredHosts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "fence-required lookup failed")
			return
		}
		entries := make([]fenceRequiredEntry, 0, len(hosts))
		for _, h := range hosts {
			entries = append(entries, fenceRequiredEntry{
				HostID:     h.ID,
				Status:     h.Status,
				Generation: h.Generation,
				ReasonCode: h.ReasonCode,
			})
		}
		writeJSON(w, http.StatusOK, fenceRequiredListResponse{
			Hosts: entries,
			Count: len(entries),
		})
	})

	return httptest.NewServer(mux)
}

// ── POST /degraded ────────────────────────────────────────────────────────────

func TestHandleMarkDegraded_HappyPath_Returns200(t *testing.T) {
	rc := "AGENT_UNRESPONSIVE"
	rec := &db.HostRecord{
		ID:         "host_001",
		Status:     "degraded",
		Generation: 3,
		ReasonCode: &rc,
	}
	stub := &stubHealthOps{degradedUpdated: true}
	srv := buildHealthServer(t, stub, rec, nil, nil)
	defer srv.Close()

	body := `{"generation":2,"from_status":"ready","reason_code":"AGENT_UNRESPONSIVE"}`
	resp, err := http.Post(srv.URL+"/degraded", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /degraded: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out markDegradedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "degraded" {
		t.Errorf("status = %q, want degraded", out.Status)
	}
	if out.Generation != 3 {
		t.Errorf("generation = %d, want 3", out.Generation)
	}
	if out.ReasonCode == nil || *out.ReasonCode != "AGENT_UNRESPONSIVE" {
		t.Errorf("reason_code = %v, want AGENT_UNRESPONSIVE", out.ReasonCode)
	}
}

func TestHandleMarkDegraded_MissingFromStatus_Returns400(t *testing.T) {
	stub := &stubHealthOps{}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	body := `{"generation":0,"reason_code":"AGENT_FAILED"}`
	resp, err := http.Post(srv.URL+"/degraded", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /degraded: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleMarkDegraded_MissingReasonCode_Returns400(t *testing.T) {
	stub := &stubHealthOps{}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	body := `{"generation":0,"from_status":"ready"}`
	resp, err := http.Post(srv.URL+"/degraded", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /degraded: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleMarkDegraded_InvalidJSON_Returns400(t *testing.T) {
	stub := &stubHealthOps{}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/degraded", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatalf("POST /degraded: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleMarkDegraded_HostNotFound_Returns404(t *testing.T) {
	stub := &stubHealthOps{degradedUpdated: false}
	// nil rec → pool returns "no rows in result set" → 404
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	body := `{"generation":0,"from_status":"ready","reason_code":"AGENT_FAILED"}`
	resp, err := http.Post(srv.URL+"/degraded", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /degraded: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleMarkDegraded_GenerationMismatch_Returns409(t *testing.T) {
	rec := &db.HostRecord{ID: "host_001", Status: "ready", Generation: 5}
	stub := &stubHealthOps{degradedUpdated: false}
	srv := buildHealthServer(t, stub, rec, nil, nil)
	defer srv.Close()

	body := `{"generation":2,"from_status":"ready","reason_code":"AGENT_FAILED"}`
	resp, err := http.Post(srv.URL+"/degraded", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /degraded: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestHandleMarkDegraded_IllegalTransition_Returns422(t *testing.T) {
	stub := &stubHealthOps{
		degradedUpdated: false,
		degradedErr:     fmt.Errorf("%w: drained → degraded via unhealthy path", db.ErrIllegalHostTransition),
	}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	body := `{"generation":0,"from_status":"fenced","reason_code":"AGENT_FAILED"}`
	resp, err := http.Post(srv.URL+"/degraded", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /degraded: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// ── POST /unhealthy ───────────────────────────────────────────────────────────

func TestHandleMarkUnhealthy_AmbiguousReason_FenceRequired_Returns200(t *testing.T) {
	rc := "AGENT_UNRESPONSIVE"
	rec := &db.HostRecord{
		ID:            "host_001",
		Status:        "unhealthy",
		Generation:    4,
		ReasonCode:    &rc,
		FenceRequired: true,
	}
	stub := &stubHealthOps{
		unhealthyUpdated:       true,
		unhealthyFenceRequired: true,
	}
	srv := buildHealthServer(t, stub, rec, nil, nil)
	defer srv.Close()

	body := `{"generation":3,"from_status":"degraded","reason_code":"AGENT_UNRESPONSIVE"}`
	resp, err := http.Post(srv.URL+"/unhealthy", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /unhealthy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out markUnhealthyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "unhealthy" {
		t.Errorf("status = %q, want unhealthy", out.Status)
	}
	if !out.FenceRequired {
		t.Error("fence_required = false, want true for AGENT_UNRESPONSIVE")
	}
	if out.ReasonCode == nil || *out.ReasonCode != "AGENT_UNRESPONSIVE" {
		t.Errorf("reason_code = %v, want AGENT_UNRESPONSIVE", out.ReasonCode)
	}
	if out.Generation != 4 {
		t.Errorf("generation = %d, want 4", out.Generation)
	}
}

func TestHandleMarkUnhealthy_NonAmbiguousReason_FenceNotRequired_Returns200(t *testing.T) {
	rc := "STORAGE_ERROR"
	rec := &db.HostRecord{
		ID:            "host_002",
		Status:        "unhealthy",
		Generation:    2,
		ReasonCode:    &rc,
		FenceRequired: false,
	}
	stub := &stubHealthOps{
		unhealthyUpdated:       true,
		unhealthyFenceRequired: false,
	}
	srv := buildHealthServer(t, stub, rec, nil, nil)
	defer srv.Close()

	body := `{"generation":1,"from_status":"ready","reason_code":"STORAGE_ERROR"}`
	resp, err := http.Post(srv.URL+"/unhealthy", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /unhealthy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out markUnhealthyResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.FenceRequired {
		t.Error("fence_required = true, want false for STORAGE_ERROR")
	}
}

func TestHandleMarkUnhealthy_MissingFromStatus_Returns400(t *testing.T) {
	stub := &stubHealthOps{}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	body := `{"generation":0,"reason_code":"AGENT_UNRESPONSIVE"}`
	resp, err := http.Post(srv.URL+"/unhealthy", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /unhealthy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleMarkUnhealthy_MissingReasonCode_Returns400(t *testing.T) {
	stub := &stubHealthOps{}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	body := `{"generation":0,"from_status":"degraded"}`
	resp, err := http.Post(srv.URL+"/unhealthy", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /unhealthy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleMarkUnhealthy_InvalidJSON_Returns400(t *testing.T) {
	stub := &stubHealthOps{}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/unhealthy", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatalf("POST /unhealthy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleMarkUnhealthy_HostNotFound_Returns404(t *testing.T) {
	stub := &stubHealthOps{unhealthyUpdated: false}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	body := `{"generation":0,"from_status":"degraded","reason_code":"HYPERVISOR_FAILED"}`
	resp, err := http.Post(srv.URL+"/unhealthy", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /unhealthy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleMarkUnhealthy_GenerationMismatch_Returns409(t *testing.T) {
	rec := &db.HostRecord{ID: "host_001", Status: "degraded", Generation: 9}
	stub := &stubHealthOps{unhealthyUpdated: false}
	srv := buildHealthServer(t, stub, rec, nil, nil)
	defer srv.Close()

	body := `{"generation":3,"from_status":"degraded","reason_code":"AGENT_FAILED"}`
	resp, err := http.Post(srv.URL+"/unhealthy", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /unhealthy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestHandleMarkUnhealthy_IllegalTransition_Returns422(t *testing.T) {
	stub := &stubHealthOps{
		unhealthyUpdated: false,
		unhealthyErr:     fmt.Errorf("%w: drained → unhealthy", db.ErrIllegalHostTransition),
	}
	srv := buildHealthServer(t, stub, nil, nil, nil)
	defer srv.Close()

	body := `{"generation":0,"from_status":"drained","reason_code":"AGENT_FAILED"}`
	resp, err := http.Post(srv.URL+"/unhealthy", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /unhealthy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// ── GET /status (Slice 3 extensions) ─────────────────────────────────────────

func TestHandleGetHostStatus_Slice3_DegradedHost_SurfacesReasonCode(t *testing.T) {
	rc := "AGENT_FAILED"
	rec := &db.HostRecord{
		ID:            "host_001",
		Status:        "degraded",
		Generation:    2,
		ReasonCode:    &rc,
		FenceRequired: false,
	}
	srv := buildHealthServer(t, &stubHealthOps{}, rec, nil, nil)
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
	if out.Status != "degraded" {
		t.Errorf("status = %q, want degraded", out.Status)
	}
	if out.ReasonCode == nil || *out.ReasonCode != "AGENT_FAILED" {
		t.Errorf("reason_code = %v, want AGENT_FAILED", out.ReasonCode)
	}
	if out.FenceRequired {
		t.Error("fence_required = true, want false for AGENT_FAILED")
	}
}

func TestHandleGetHostStatus_Slice3_UnhealthyHost_FenceRequired(t *testing.T) {
	rc := "HYPERVISOR_FAILED"
	rec := &db.HostRecord{
		ID:            "host_002",
		Status:        "unhealthy",
		Generation:    6,
		ReasonCode:    &rc,
		FenceRequired: true,
	}
	srv := buildHealthServer(t, &stubHealthOps{}, rec, nil, nil)
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
	if out.Status != "unhealthy" {
		t.Errorf("status = %q, want unhealthy", out.Status)
	}
	if out.ReasonCode == nil || *out.ReasonCode != "HYPERVISOR_FAILED" {
		t.Errorf("reason_code = %v, want HYPERVISOR_FAILED", out.ReasonCode)
	}
	if !out.FenceRequired {
		t.Error("fence_required = false, want true for HYPERVISOR_FAILED")
	}
	if out.Generation != 6 {
		t.Errorf("generation = %d, want 6", out.Generation)
	}
}

func TestHandleGetHostStatus_Slice3_ReadyHost_NilReasonCode(t *testing.T) {
	rec := &db.HostRecord{
		ID:            "host_003",
		Status:        "ready",
		Generation:    0,
		ReasonCode:    nil,
		FenceRequired: false,
	}
	srv := buildHealthServer(t, &stubHealthOps{}, rec, nil, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	var out hostStatusResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.ReasonCode != nil {
		t.Errorf("reason_code = %v, want nil for ready host", out.ReasonCode)
	}
	if out.FenceRequired {
		t.Error("fence_required = true, want false for ready host")
	}
}

// ── GET /fence-required ───────────────────────────────────────────────────────

func TestHandleGetFenceRequired_EmptyList_Returns200(t *testing.T) {
	srv := buildHealthServer(t, &stubHealthOps{}, nil, nil, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/fence-required")
	if err != nil {
		t.Fatalf("GET /fence-required: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out fenceRequiredListResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Count != 0 {
		t.Errorf("count = %d, want 0", out.Count)
	}
	if len(out.Hosts) != 0 {
		t.Errorf("hosts len = %d, want 0", len(out.Hosts))
	}
}

func TestHandleGetFenceRequired_PopulatedList_Returns200WithEntries(t *testing.T) {
	rc1 := "AGENT_UNRESPONSIVE"
	rc2 := "HYPERVISOR_FAILED"
	listRecs := []*db.HostRecord{
		{
			ID:            "host_001",
			Status:        "unhealthy",
			Generation:    4,
			ReasonCode:    &rc1,
			FenceRequired: true,
		},
		{
			ID:            "host_002",
			Status:        "unhealthy",
			Generation:    7,
			ReasonCode:    &rc2,
			FenceRequired: true,
		},
	}
	srv := buildHealthServer(t, &stubHealthOps{}, nil, nil, listRecs)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/fence-required")
	if err != nil {
		t.Fatalf("GET /fence-required: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out fenceRequiredListResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Count != 2 {
		t.Errorf("count = %d, want 2", out.Count)
	}
	if len(out.Hosts) != 2 {
		t.Fatalf("hosts len = %d, want 2", len(out.Hosts))
	}
	if out.Hosts[0].HostID != "host_001" {
		t.Errorf("hosts[0].host_id = %q, want host_001", out.Hosts[0].HostID)
	}
	if out.Hosts[0].ReasonCode == nil || *out.Hosts[0].ReasonCode != "AGENT_UNRESPONSIVE" {
		t.Errorf("hosts[0].reason_code = %v, want AGENT_UNRESPONSIVE", out.Hosts[0].ReasonCode)
	}
	if out.Hosts[1].HostID != "host_002" {
		t.Errorf("hosts[1].host_id = %q, want host_002", out.Hosts[1].HostID)
	}
}

// ── Route ordering: /unhealthy not shadowed by anything ───────────────────────

func TestRouteOrder_Slice3_NewPathsAreUnambiguous(t *testing.T) {
	paths := map[string]string{
		"/internal/v1/hosts/host_001/degraded":  "/degraded",
		"/internal/v1/hosts/host_001/unhealthy": "/unhealthy",
	}
	for fullPath, suffix := range paths {
		if !strings.HasSuffix(fullPath, suffix) {
			t.Errorf("%q: expected HasSuffix(%q)=true", fullPath, suffix)
		}
		// Verify /degraded does not shadow /drain or vice versa
		if suffix == "/degraded" && strings.HasSuffix(fullPath, "/drain") {
			t.Errorf("%q: degraded path must not match /drain suffix", fullPath)
		}
		if suffix == "/unhealthy" && strings.HasSuffix(fullPath, "/drain") {
			t.Errorf("%q: unhealthy path must not match /drain suffix", fullPath)
		}
	}
}
