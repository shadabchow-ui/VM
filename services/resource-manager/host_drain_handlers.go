package main

// host_drain_handlers.go — HTTP handlers for the host lifecycle drain API.
//
// VM-P2E Slice 1: POST /drain, GET /status.
// VM-P2E Slice 2:
//   - Fixed handleDrainHost: dead code removed; req.Generation now forwarded
//     to DrainHost (was always 0); response now includes real generation and
//     drain_reason from GetHostByID.
//   - Added handleCompleteDrainHost: POST /internal/v1/hosts/{id}/drain-complete
//     — explicit draining→drained transition, guarded by active-workload check.
//   - hostStatusResponse now includes real generation and drain_reason values.
//
// Endpoints (all /internal/v1/hosts/{host_id}/...):
//   POST .../drain          — mark host draining, detach stopped VMs
//   POST .../drain-complete — attempt draining→drained (blocked if active VMs remain)
//   GET  .../status         — observable host lifecycle state
//
// Auth: mTLS required (enforced by RequireMTLS middleware in api.go routes).
// These endpoints are operator/control-plane tools, not user-facing.
//
// Design:
//   - All state transitions are generation-checked (CAS). Omitting or sending
//     a wrong generation value returns 409 Conflict.
//   - POST drain is idempotent within a generation: repeated requests with the
//     current generation succeed if the host is already draining.
//   - POST drain-complete returns 202 Accepted when blocked by active workload,
//     with the remaining active count in the body. Returns 200 OK when the host
//     has transitioned to drained.
//   - GET status always reflects the real current state from DB.
//
// Source: vm-13-03__blueprint__ §components "Fleet Management Service",
//         §core_contracts "Host State Atomicity",
//         §interaction_or_ops_contract "Operator initiates/confirms single-host drain".

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ── Request / Response types ──────────────────────────────────────────────────

// drainRequest is the payload for POST /internal/v1/hosts/{host_id}/drain.
type drainRequest struct {
	// Generation is the caller's expected current generation of the host record.
	// Required: prevents concurrent actors from racing on status transitions.
	// Obtain the current generation from GET /internal/v1/hosts/{id}/status first.
	// Source: vm-13-03__blueprint__ §implementation_decisions generation enforcement.
	Generation int64 `json:"generation"`
	// Reason is an optional human-readable description of why the drain was initiated.
	// Stored for observability. Not required.
	Reason *string `json:"reason,omitempty"`
}

// drainResponse is returned by POST /internal/v1/hosts/{host_id}/drain.
type drainResponse struct {
	HostID               string  `json:"host_id"`
	Status               string  `json:"status"`
	Generation           int64   `json:"generation"`
	DrainReason          *string `json:"drain_reason,omitempty"`
	RunningInstanceCount int     `json:"running_instance_count"`
	DetachedStoppedCount int64   `json:"detached_stopped_count"` // reserved; always 0 in Phase 1
	// FullyDrained is true when no active instances remain on the host after
	// the drain command. The host status is still 'draining' until
	// POST .../drain-complete is explicitly called and succeeds.
	FullyDrained bool `json:"fully_drained"`
}

// drainCompleteRequest is the payload for POST .../drain-complete.
type drainCompleteRequest struct {
	// Generation is the current generation of the host record (post-drain).
	// Obtain from GET /status or from the drain response.
	Generation int64 `json:"generation"`
}

// drainCompleteResponse is returned by POST .../drain-complete.
type drainCompleteResponse struct {
	HostID      string  `json:"host_id"`
	Status      string  `json:"status"`
	Generation  int64   `json:"generation"`
	DrainReason *string `json:"drain_reason,omitempty"`
	// ActiveInstanceCount is non-zero when the drain-complete was blocked.
	// In that case, HTTP status is 202 Accepted and Status is still 'draining'.
	ActiveInstanceCount int  `json:"active_instance_count"`
	Completed           bool `json:"completed"`
}

// hostStatusResponse is returned by GET /internal/v1/hosts/{host_id}/status.
type hostStatusResponse struct {
	HostID      string  `json:"host_id"`
	Status      string  `json:"status"`
	Generation  int64   `json:"generation"`
	DrainReason *string `json:"drain_reason,omitempty"`
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// handleDrainHost handles POST /internal/v1/hosts/{host_id}/drain.
//
// Steps:
//  1. Decode drain request (generation required, reason optional).
//  2. Call inventory.DrainHost — CAS status transition + stopped-VM detach.
//  3. Re-read host to get the post-transition generation and drain_reason.
//  4. Return 200 + drainResponse with observable drain state.
//
// Error mapping:
//   - 400: invalid JSON.
//   - 404: host not found (detected via GetHostByID after CAS failure).
//   - 409: generation mismatch (CAS failed and host exists).
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

	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}

	runningCount, updated, err := s.inventory.DrainHost(r.Context(), hostID, req.Generation, reason)
	if err != nil {
		s.log.Error("DrainHost failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "drain failed: "+err.Error())
		return
	}

	if !updated {
		// CAS failed: determine whether the host exists or the generation is wrong.
		host, lookupErr := s.inventory.repo.GetHostByID(r.Context(), hostID)
		if lookupErr != nil || host == nil {
			writeError(w, http.StatusNotFound, "host not found: "+hostID)
			return
		}
		writeError(w, http.StatusConflict, "generation mismatch or host not in a drainable state")
		return
	}

	// Re-read the host to get the post-transition generation and stored drain_reason.
	host, err := s.inventory.repo.GetHostByID(r.Context(), hostID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "status lookup failed after drain")
		return
	}

	writeJSON(w, http.StatusOK, drainResponse{
		HostID:               host.ID,
		Status:               host.Status,
		Generation:           host.Generation,
		DrainReason:          host.DrainReason,
		RunningInstanceCount: runningCount,
		DetachedStoppedCount: 0, // reserved for future observability
		FullyDrained:         runningCount == 0,
	})
}

// handleCompleteDrainHost handles POST /internal/v1/hosts/{host_id}/drain-complete.
//
// Attempts the draining → drained transition.
// Blocked (returns 202 Accepted) if active VM workload still remains on the host.
// Succeeds (returns 200 OK) when the host becomes drained.
//
// This endpoint is idempotent: once the host is drained, subsequent calls
// return 409 because the status='draining' guard no longer matches.
// Callers should re-read /status to confirm the current state.
//
// Error mapping:
//   - 400: invalid JSON.
//   - 404: host not found.
//   - 409: generation mismatch or host not in 'draining' state.
//   - 202: active instances remain; drain not yet complete.
//   - 200: host transitioned to 'drained'.
//   - 500: unexpected DB error.
//
// Source: vm-13-03__blueprint__ §interaction_or_ops_contract
//         "Operator confirms drain complete".
func (s *server) handleCompleteDrainHost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostID := extractPathSegment(r.URL.Path, "hosts", "drain-complete")
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
		return
	}

	var req drainCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	activeCount, updated, err := s.inventory.CompleteDrain(r.Context(), hostID, req.Generation)
	if err != nil {
		s.log.Error("CompleteDrain failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "drain-complete failed: "+err.Error())
		return
	}

	if !updated && activeCount == 0 {
		// Zero active workload but CAS matched 0 rows: generation mismatch,
		// host not in draining state, or host not found. Distinguish cases.
		host, lookupErr := s.inventory.repo.GetHostByID(r.Context(), hostID)
		if lookupErr != nil || host == nil {
			writeError(w, http.StatusNotFound, "host not found: "+hostID)
			return
		}
		// Host exists but is not in draining state (or generation wrong).
		writeError(w, http.StatusConflict, "host is not in draining state or generation mismatch")
		return
	}

	if activeCount > 0 {
		// Blocked: return 202 Accepted with the blocking count.
		// Re-read for current generation to give caller an accurate retry handle.
		host, _ := s.inventory.repo.GetHostByID(r.Context(), hostID)
		var gen int64
		var drainReason *string
		var currentStatus = "draining"
		if host != nil {
			gen = host.Generation
			drainReason = host.DrainReason
			currentStatus = host.Status
		}
		writeJSON(w, http.StatusAccepted, drainCompleteResponse{
			HostID:              hostID,
			Status:              currentStatus,
			Generation:          gen,
			DrainReason:         drainReason,
			ActiveInstanceCount: activeCount,
			Completed:           false,
		})
		return
	}

	// Transition succeeded. Re-read to get the new generation.
	host, err := s.inventory.repo.GetHostByID(r.Context(), hostID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "status lookup failed after drain-complete")
		return
	}

	s.log.Info("host drained", "host_id", hostID, "generation", host.Generation)

	writeJSON(w, http.StatusOK, drainCompleteResponse{
		HostID:              host.ID,
		Status:              host.Status,
		Generation:          host.Generation,
		DrainReason:         host.DrainReason,
		ActiveInstanceCount: 0,
		Completed:           true,
	})
}

// handleGetHostStatus handles GET /internal/v1/hosts/{host_id}/status.
//
// Returns the current lifecycle state of a host for observability.
// VM-P2E Slice 2: now returns real generation and drain_reason values
// (Slice 1 always returned generation=0 and drain_reason=nil).
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
		if strings.Contains(err.Error(), "host not found") {
			writeError(w, http.StatusNotFound, "host not found: "+hostID)
			return
		}
		s.log.Error("GetHostStatus failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "status lookup failed")
		return
	}

	writeJSON(w, http.StatusOK, hostStatusResponse{
		HostID:      host.ID,
		Status:      host.Status,
		Generation:  host.Generation,
		DrainReason: host.DrainReason,
	})
}
