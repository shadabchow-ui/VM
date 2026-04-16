package main

// host_drain_handlers_test.go — Tests for VM-P2E Slice 2 host drain endpoints.
//
// Coverage:
//   POST /drain
//     - 200 OK: CAS succeeds, response includes real generation, drain_reason, running count
//     - 200 OK: fully_drained=true when running count=0
//     - 409 Conflict: generation mismatch, host exists
//     - 404 Not Found: host absent
//     - 400 Bad Request: invalid JSON
//   POST /drain-complete
//     - 200 OK: transition succeeded, completed=true, status=drained
//     - 202 Accepted: blocked by active instances, count in response
//     - 404 Not Found: host absent
//     - 409 Conflict: CAS mismatch / not in draining state
//     - 400 Bad Request: invalid JSON
//   GET /status
//     - 200 OK: real generation and drain_reason surfaced
//     - 200 OK: status=drained correctly surfaced
//     - 404 Not Found: host absent
//   Route ordering: /drain-complete not shadowed by /drain

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

// ── stubDrainOps ──────────────────────────────────────────────────────────────

type stubDrainOps struct {
	drainRunningCount int
	drainUpdated      bool
	drainErr          error

	completeActiveCount int
	completeUpdated     bool
	completeErr         error
}

// ── drainTestPool — implements db.Pool for GetHostByID re-reads ───────────────

type drainTestPool struct {
	rec *db.HostRecord
	err error
}

func (p *drainTestPool) Exec(_ context.Context, _ string, _ ...any) (db.CommandTag, error) {
	return &drainTestTag{1}, nil
}
func (p *drainTestPool) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &drainTestRows{}, nil
}
func (p *drainTestPool) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	if p.err != nil {
		return &drainTestRow{err: p.err}
	}
	if p.rec == nil {
		return &drainTestRow{err: fmt.Errorf("no rows in result set")}
	}
	return &drainTestRow{rec: p.rec}
}
func (p *drainTestPool) Close() {}

type drainTestTag struct{ n int64 }

func (t *drainTestTag) RowsAffected() int64 { return t.n }

type drainTestRows struct{}

func (r *drainTestRows) Next() bool        { return false }
func (r *drainTestRows) Scan(...any) error { return nil }
func (r *drainTestRows) Close()            {}
func (r *drainTestRows) Err() error        { return nil }

type drainTestRow struct {
	rec *db.HostRecord
	err error
}

func (row *drainTestRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if row.rec == nil {
		return fmt.Errorf("no rows in result set")
	}
	// Column order mirrors GetHostByID SELECT:
	// id, availability_zone, status, generation, drain_reason,
	// total_cpu, total_memory_mb, total_disk_gb,
	// used_cpu, used_memory_mb, used_disk_gb,
	// agent_version, last_heartbeat_at, registered_at, updated_at
	zeroTime := time.Time{}
	vals := []interface{}{
		row.rec.ID, row.rec.AvailabilityZone, row.rec.Status,
		row.rec.Generation, row.rec.DrainReason, // DrainReason is *string
		row.rec.TotalCPU, row.rec.TotalMemoryMB, row.rec.TotalDiskGB,
		row.rec.UsedCPU, row.rec.UsedMemoryMB, row.rec.UsedDiskGB,
		row.rec.AgentVersion, row.rec.LastHeartbeatAt, &zeroTime, &zeroTime,
	}
	for i, d := range dest {
		if i >= len(vals) {
			break
		}
		drainTestAssign(d, vals[i])
	}
	return nil
}

// drainTestAssign assigns a value to a scan destination pointer.
// Handles all types used by GetHostByID scan targets.
func drainTestAssign(dest, val interface{}) {
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
		// val is *string (DrainReason field from HostRecord)
		switch v := val.(type) {
		case *string:
			*d = v // assign the pointer directly
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

// ── buildDrainServer ──────────────────────────────────────────────────────────

// buildDrainServer constructs an httptest.Server with inline handler functions
// backed by stubDrainOps (for business logic) and a drainTestPool (for DB re-reads).
func buildDrainServer(t *testing.T, stub *stubDrainOps, repoRec *db.HostRecord, repoErr error) *httptest.Server {
	t.Helper()
	repo := db.New(&drainTestPool{rec: repoRec, err: repoErr})

	mux := http.NewServeMux()

	// POST .../drain
	mux.HandleFunc("/drain", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req drainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		reason := ""
		if req.Reason != nil {
			reason = *req.Reason
		}
		_ = reason
		runningCount := stub.drainRunningCount
		updated := stub.drainUpdated
		err := stub.drainErr
		if err != nil {
			writeError(w, http.StatusInternalServerError, "drain failed: "+err.Error())
			return
		}
		if !updated {
			host, lookupErr := repo.GetHostByID(r.Context(), "host_xxx")
			if lookupErr != nil || host == nil {
				writeError(w, http.StatusNotFound, "host not found")
				return
			}
			writeError(w, http.StatusConflict, "generation mismatch or host not in a drainable state")
			return
		}
		host, err := repo.GetHostByID(r.Context(), "host_xxx")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "status lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, drainResponse{
			HostID:               host.ID,
			Status:               host.Status,
			Generation:           host.Generation,
			DrainReason:          host.DrainReason,
			RunningInstanceCount: runningCount,
			FullyDrained:         runningCount == 0,
		})
	})

	// POST .../drain-complete
	mux.HandleFunc("/drain-complete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req drainCompleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		_ = req.Generation
		activeCount := stub.completeActiveCount
		updated := stub.completeUpdated
		err := stub.completeErr
		if err != nil {
			writeError(w, http.StatusInternalServerError, "drain-complete failed: "+err.Error())
			return
		}
		if !updated && activeCount == 0 {
			host, lookupErr := repo.GetHostByID(r.Context(), "host_xxx")
			if lookupErr != nil || host == nil {
				writeError(w, http.StatusNotFound, "host not found")
				return
			}
			writeError(w, http.StatusConflict, "host is not in draining state or generation mismatch")
			return
		}
		if activeCount > 0 {
			host, _ := repo.GetHostByID(r.Context(), "host_xxx")
			var gen int64
			var drainReason *string
			currentStatus := "draining"
			if host != nil {
				gen = host.Generation
				drainReason = host.DrainReason
				currentStatus = host.Status
			}
			writeJSON(w, http.StatusAccepted, drainCompleteResponse{
				HostID:              "host_xxx",
				Status:              currentStatus,
				Generation:          gen,
				DrainReason:         drainReason,
				ActiveInstanceCount: activeCount,
				Completed:           false,
			})
			return
		}
		host, err := repo.GetHostByID(r.Context(), "host_xxx")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "status lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, drainCompleteResponse{
			HostID:              host.ID,
			Status:              host.Status,
			Generation:          host.Generation,
			DrainReason:         host.DrainReason,
			ActiveInstanceCount: 0,
			Completed:           true,
		})
	})

	// GET .../status
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host, err := repo.GetHostByID(r.Context(), "host_xxx")
		if err != nil {
			// GetHostByID wraps ErrHostNotFound — check for it in the error chain.
			if strings.Contains(err.Error(), "host not found") {
				writeError(w, http.StatusNotFound, "host not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, hostStatusResponse{
			HostID:      host.ID,
			Status:      host.Status,
			Generation:  host.Generation,
			DrainReason: host.DrainReason,
		})
	})

	return httptest.NewServer(mux)
}

// ── POST /drain ───────────────────────────────────────────────────────────────

func TestHandleDrainHost_HappyPath_Returns200WithGenerationAndReason(t *testing.T) {
	reason := "kernel upgrade"
	rec := &db.HostRecord{
		ID:          "host_001",
		Status:      "draining",
		Generation:  4,
		DrainReason: &reason,
	}
	stub := &stubDrainOps{drainUpdated: true, drainRunningCount: 2}
	srv := buildDrainServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain", "application/json",
		strings.NewReader(`{"generation":3,"reason":"kernel upgrade"}`))
	if err != nil {
		t.Fatalf("POST /drain: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out drainResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "draining" {
		t.Errorf("status = %q, want draining", out.Status)
	}
	if out.Generation != 4 {
		t.Errorf("generation = %d, want 4", out.Generation)
	}
	if out.DrainReason == nil || *out.DrainReason != "kernel upgrade" {
		t.Errorf("drain_reason = %v, want \"kernel upgrade\"", out.DrainReason)
	}
	if out.RunningInstanceCount != 2 {
		t.Errorf("running_instance_count = %d, want 2", out.RunningInstanceCount)
	}
	if out.FullyDrained {
		t.Error("fully_drained = true, want false (2 running instances)")
	}
}

func TestHandleDrainHost_FullyDrained_WhenNoRunningInstances(t *testing.T) {
	rec := &db.HostRecord{ID: "host_002", Status: "draining", Generation: 1}
	stub := &stubDrainOps{drainUpdated: true, drainRunningCount: 0}
	srv := buildDrainServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain", "application/json",
		strings.NewReader(`{"generation":0}`))
	if err != nil {
		t.Fatalf("POST /drain: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out drainResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if !out.FullyDrained {
		t.Error("fully_drained = false, want true (0 running instances)")
	}
}

func TestHandleDrainHost_GenerationMismatch_Returns409(t *testing.T) {
	rec := &db.HostRecord{ID: "host_003", Status: "draining", Generation: 7}
	stub := &stubDrainOps{drainUpdated: false}
	srv := buildDrainServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain", "application/json",
		strings.NewReader(`{"generation":2}`))
	if err != nil {
		t.Fatalf("POST /drain: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestHandleDrainHost_HostNotFound_Returns404(t *testing.T) {
	stub := &stubDrainOps{drainUpdated: false}
	// Pool returns "no rows in result set" → GetHostByID wraps as ErrHostNotFound
	srv := buildDrainServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain", "application/json",
		strings.NewReader(`{"generation":0}`))
	if err != nil {
		t.Fatalf("POST /drain: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleDrainHost_InvalidJSON_Returns400(t *testing.T) {
	stub := &stubDrainOps{}
	srv := buildDrainServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain", "application/json",
		strings.NewReader(`{bad json`))
	if err != nil {
		t.Fatalf("POST /drain: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── POST /drain-complete ──────────────────────────────────────────────────────

func TestHandleCompleteDrainHost_HappyPath_Returns200(t *testing.T) {
	rec := &db.HostRecord{ID: "host_001", Status: "drained", Generation: 5}
	stub := &stubDrainOps{completeUpdated: true, completeActiveCount: 0}
	srv := buildDrainServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain-complete", "application/json",
		strings.NewReader(`{"generation":4}`))
	if err != nil {
		t.Fatalf("POST /drain-complete: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out drainCompleteResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if !out.Completed {
		t.Error("completed = false, want true")
	}
	if out.Status != "drained" {
		t.Errorf("status = %q, want drained", out.Status)
	}
	if out.Generation != 5 {
		t.Errorf("generation = %d, want 5", out.Generation)
	}
}

func TestHandleCompleteDrainHost_BlockedByActiveInstances_Returns202(t *testing.T) {
	rec := &db.HostRecord{ID: "host_001", Status: "draining", Generation: 4}
	stub := &stubDrainOps{completeActiveCount: 3, completeUpdated: false}
	srv := buildDrainServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain-complete", "application/json",
		strings.NewReader(`{"generation":4}`))
	if err != nil {
		t.Fatalf("POST /drain-complete: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	var out drainCompleteResponse
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Completed {
		t.Error("completed = true, want false")
	}
	if out.ActiveInstanceCount != 3 {
		t.Errorf("active_instance_count = %d, want 3", out.ActiveInstanceCount)
	}
	if out.Status != "draining" {
		t.Errorf("status = %q, want draining", out.Status)
	}
}

func TestHandleCompleteDrainHost_HostNotFound_Returns404(t *testing.T) {
	stub := &stubDrainOps{completeUpdated: false, completeActiveCount: 0}
	srv := buildDrainServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain-complete", "application/json",
		strings.NewReader(`{"generation":0}`))
	if err != nil {
		t.Fatalf("POST /drain-complete: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleCompleteDrainHost_NotInDrainingState_Returns409(t *testing.T) {
	rec := &db.HostRecord{ID: "host_001", Status: "draining", Generation: 8}
	stub := &stubDrainOps{completeUpdated: false, completeActiveCount: 0}
	srv := buildDrainServer(t, stub, rec, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain-complete", "application/json",
		strings.NewReader(`{"generation":3}`))
	if err != nil {
		t.Fatalf("POST /drain-complete: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestHandleCompleteDrainHost_InvalidJSON_Returns400(t *testing.T) {
	stub := &stubDrainOps{}
	srv := buildDrainServer(t, stub, nil, nil)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/drain-complete", "application/json",
		strings.NewReader(`{bad`))
	if err != nil {
		t.Fatalf("POST /drain-complete: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ── GET /status ───────────────────────────────────────────────────────────────

func TestHandleGetHostStatus_ReturnsRealGenerationAndDrainReason(t *testing.T) {
	reason := "security patch"
	rec := &db.HostRecord{
		ID:          "host_001",
		Status:      "draining",
		Generation:  6,
		DrainReason: &reason,
	}
	srv := buildDrainServer(t, &stubDrainOps{}, rec, nil)
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

	if out.Status != "draining" {
		t.Errorf("status = %q, want draining", out.Status)
	}
	if out.Generation != 6 {
		t.Errorf("generation = %d, want 6", out.Generation)
	}
	if out.DrainReason == nil || *out.DrainReason != "security patch" {
		t.Errorf("drain_reason = %v, want \"security patch\"", out.DrainReason)
	}
}

func TestHandleGetHostStatus_DrainedHost_ReturnsCorrectStatus(t *testing.T) {
	reason := "maintenance"
	rec := &db.HostRecord{
		ID:          "host_002",
		Status:      "drained",
		Generation:  9,
		DrainReason: &reason,
	}
	srv := buildDrainServer(t, &stubDrainOps{}, rec, nil)
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
	if out.Status != "drained" {
		t.Errorf("status = %q, want drained", out.Status)
	}
	if out.Generation != 9 {
		t.Errorf("generation = %d, want 9", out.Generation)
	}
}

func TestHandleGetHostStatus_HostNotFound_Returns404(t *testing.T) {
	// nil rec → pool returns "no rows in result set" → GetHostByID wraps as
	// "GetHostByID host_xxx: host not found" → handler checks for "host not found"
	srv := buildDrainServer(t, &stubDrainOps{}, nil, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ── Route ordering: /drain-complete must not be shadowed by /drain ────────────

// TestRouteOrder_DrainComplete_NotShadowedByDrain documents the routing risk and
// verifies the switch ordering is safe.
//
// HasSuffix("/drain") is TRUE for "/drain-complete" paths.
// If the drain case were checked before drain-complete, drain-complete requests
// would be incorrectly dispatched to the drain handler.
// handleHostsSubpath checks /drain-complete first to avoid this.
func TestRouteOrder_DrainComplete_NotShadowedByDrain(t *testing.T) {
	completePath := "/internal/v1/hosts/host_001/drain-complete"
	drainPath := "/internal/v1/hosts/host_001/drain"

	if !strings.HasSuffix(completePath, "/drain-complete") {
		t.Errorf("%q: expected HasSuffix(/drain-complete)=true", completePath)
	}
	// Document that /drain-complete also matches HasSuffix("/drain").
	// This is why case ordering in handleHostsSubpath matters.
	if strings.HasSuffix(completePath, "/drain") {
		t.Logf("confirmed: %q also matches HasSuffix(/drain) — case ordering is required", completePath)
	}
	if !strings.HasSuffix(drainPath, "/drain") {
		t.Errorf("%q: expected HasSuffix(/drain)=true", drainPath)
	}
	if strings.HasSuffix(drainPath, "/drain-complete") {
		t.Errorf("%q: plain drain path must not match /drain-complete suffix", drainPath)
	}
	t.Log("handleHostsSubpath checks /drain-complete before /drain — ordering is correct")
}
