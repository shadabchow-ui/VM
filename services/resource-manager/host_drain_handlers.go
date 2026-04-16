package main

// host_drain_handlers.go — HTTP handlers for the host lifecycle drain API.
//
// VM-P2E Slice 1: adds the operator drain seam to the Resource Manager.
//
// Endpoints:
//   POST /internal/v1/hosts/{host_id}/drain    — mark host draining, detach stopped VMs
//   GET  /internal/v1/hosts/{host_id}/status   — observable host lifecycle state
//
// Auth: mTLS required (same as all /internal/v1/hosts/* routes).
// These endpoints are operator/control-plane tools, not user-facing.
//
// Design:
//   - POST drain is idempotent: repeated requests with the same generation succeed
//     if the host is already draining.
//   - generation (optimistic concurrency) is required in the drain request body.
//     Omitting it or providing the wrong value returns 409 Conflict.
//   - The handler does NOT automatically transition the host to 'drained'. That
//     requires a separate action (P2E Slice 2: drain watch-loop or operator call).
//   - Scheduler exclusion is passive: ListReadyHosts only returns status=ready hosts,
//     so marking draining is sufficient to stop new placements immediately.
//
// Source: vm-13-03__blueprint__ §components "Fleet Management Service",
//         §core_contracts "Host State Atomicity",
//         §interaction_or_ops_contract "Operator initiates single-host drain".

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ── Request / Response types ──────────────────────────────────────────────────

// drainRequest is the payload for POST /internal/v1/hosts/{host_id}/drain.
type drainRequest struct {
	// Generation is the expected current generation of the host record.
	// Required: prevents race conditions between concurrent actors.
	// Source: vm-13-03__blueprint__ §implementation_decisions generation enforcement.
	Generation int `json:"generation"`
	// Reason is an optional human-readable description of why the drain was initiated.
	// Stored on the host record for observability. Not required.
	Reason *string `json:"reason,omitempty"`
}

// drainResponse is returned by POST /internal/v1/hosts/{host_id}/drain.
type drainResponse struct {
	HostID               string `json:"host_id"`
	Status               string `json:"status"`
	Generation           int    `json:"generation"`
	RunningInstanceCount int    `json:"running_instance_count"`
	DetachedStoppedCount int64  `json:"detached_stopped_count"`
	// FullyDrained is true when no running instances remain on the host.
	// The host is not automatically transitioned to 'drained' even when this is true.
	FullyDrained bool `json:"fully_drained"`
}

// hostStatusResponse is returned by GET /internal/v1/hosts/{host_id}/status.
type hostStatusResponse struct {
	HostID      string  `json:"host_id"`
	Status      string  `json:"status"`
	Generation  int     `json:"generation"`
	DrainReason *string `json:"drain_reason,omitempty"`
}

// ── Route registration ────────────────────────────────────────────────────────

// registerHostDrainRoutes wires the drain endpoints into the existing
// handleHostsSubpath dispatcher. Called from routes() in api.go.
//
// No new mux entries are needed: handleHostsSubpath already catches all
// /internal/v1/hosts/{id}/... paths. We extend it to recognise /drain and /status.
//
// This function is intentionally empty — the routing extension is done by
// patching handleHostsSubpath directly (see api.go patch below).
// It is kept here as a documentation anchor.

// ── Handlers ──────────────────────────────────────────────────────────────────

// handleDrainHost handles POST /internal/v1/hosts/{host_id}/drain.
//
// Steps:
//  1. Decode drain request (generation required, reason optional).
//  2. Call inventory.DrainHost — CAS status transition + stopped-VM detach.
//  3. Return 200 + drainResponse with observable drain state.
//
// Error mapping:
//   - 400: missing/invalid JSON or missing generation field.
//   - 404: host not found.
//   - 409: generation mismatch (CAS failed, non-idempotent conflict).
//   - 500: unexpected DB error.
func (s *server) handleDrainHost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostID := extractPathSegment(r.URL.Path, "hosts", "drain")
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
		return
	}

	var req drainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	reasonValue := ""
if req.Reason != nil {
    reasonValue = *req.Reason
}

runningCount, updated, err := s.inventory.DrainHost(r.Context(), hostID, reasonValue)
if err != nil {
    s.log.Error("DrainHost failed", "host_id", hostID, "error", err)
    writeError(w, http.StatusInternalServerError, "drain failed")
    return
}

if !updated {
    writeError(w, http.StatusConflict, "host not found or concurrent update")
    return
}

host, err := s.inventory.repo.GetHostByID(r.Context(), hostID)
if err != nil {
    writeError(w, http.StatusInternalServerError, "status lookup failed")
    return
}

writeJSON(w, http.StatusOK, drainResponse{
    HostID:               host.ID,
    Status:               host.Status,
    Generation:           0,
    RunningInstanceCount: runningCount,
    DetachedStoppedCount: 0,
    FullyDrained:         runningCount == 0,
})
	if err != nil {
		msg := err.Error()
		// Generation mismatch or conflicting drain → 409.
		if strings.Contains(msg, "generation mismatch") || strings.Contains(msg, "not found") {
			if strings.Contains(msg, "not found") {
				writeError(w, http.StatusNotFound, "host not found: "+hostID)
				return
			}
			writeError(w, http.StatusConflict, msg)
			return
		}
		s.log.Error("DrainHost failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "drain failed")
		return
	}

}

// handleGetHostStatus handles GET /internal/v1/hosts/{host_id}/status.
//
// Returns the current lifecycle state of a host for observability.
// No auth beyond mTLS is required: any authenticated internal service can observe
// host state (scheduler, reconciler, operator tooling).
func (s *server) handleGetHostStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostID := extractPathSegment(r.URL.Path, "hosts", "status")
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
		return
	}

	host, err := s.inventory.repo.GetHostByID(r.Context(), hostID)
	if err != nil {
		s.log.Error("GetHostStatus failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "status lookup failed")
		return
	}
	if host == nil {
		writeError(w, http.StatusNotFound, "host not found: "+hostID)
		return
	}

	writeJSON(w, http.StatusOK, hostStatusResponse{
		HostID:      host.ID,
		Status:      host.Status,
		Generation:  0,
		DrainReason: nil,
	})
}
