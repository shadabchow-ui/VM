package main

// host_drain_handlers.go — HTTP handlers for the host lifecycle drain and health API.
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
// VM-P2E Slice 3:
//   - hostStatusResponse now includes reason_code and fence_required fields.
//   - handleMarkDegraded: POST /internal/v1/hosts/{id}/degraded
//     — transition to 'degraded' with reason_code; generation-checked CAS.
//   - handleMarkUnhealthy: POST /internal/v1/hosts/{id}/unhealthy
//     — transition to 'unhealthy' with reason_code; sets fence_required for
//     ambiguous failure reason codes.
//   - handleGetFenceRequired: GET /internal/v1/hosts/fence-required
//     — observable list of hosts with fence_required=TRUE (fencing groundwork).
//
// Endpoints (all /internal/v1/hosts/{host_id}/... unless noted):
//   POST .../drain           — mark host draining, detach stopped VMs (Slice 1)
//   POST .../drain-complete  — attempt draining→drained (blocked if active VMs) (Slice 2)
//   POST .../degraded        — mark host degraded with reason_code (Slice 3)
//   POST .../unhealthy       — mark host unhealthy with reason_code; sets fence_required (Slice 3)
//   GET  .../status          — observable host lifecycle state (Slice 1/2/3)
//   GET  /internal/v1/hosts/fence-required — list hosts needing fencing (Slice 3)
//
// Auth: mTLS required (enforced by RequireMTLS middleware in api.go routes).
// These endpoints are operator/control-plane tools, not user-facing.
//
// Design:
//   - All state transitions are generation-checked (CAS). Omitting or sending
//     a wrong generation value returns 409 Conflict.
//   - Illegal state transitions (e.g., drained → unhealthy) return 422.
//   - POST degraded/unhealthy require fromStatus in the request body so the
//     transition can be validated against the legal transition table without
//     a round-trip DB read in the hot path.
//   - GET status always reflects the real current state from DB.
//   - GET fence-required is a pure read; safe for polling by operator tooling.
//
// Source: vm-13-03__blueprint__ §components "Fleet Management Service",
//         §core_contracts "Host State Atomicity",
//         §interaction_or_ops_contract "Operator initiates/confirms single-host drain",
//         §"Fencing Decision Logic".

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
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
//
// VM-P2E Slice 3: added ReasonCode and FenceRequired fields.
//   - reason_code: non-nil for hosts in degraded or unhealthy state; nil otherwise.
//   - fence_required: true when the host's failure is ambiguous enough that
//     fencing must complete before recovery automation may proceed.
type hostStatusResponse struct {
	HostID        string  `json:"host_id"`
	Status        string  `json:"status"`
	Generation    int64   `json:"generation"`
	DrainReason   *string `json:"drain_reason,omitempty"`
	ReasonCode    *string `json:"reason_code,omitempty"`
	FenceRequired bool    `json:"fence_required"`
}

// markDegradedRequest is the payload for POST /internal/v1/hosts/{host_id}/degraded.
type markDegradedRequest struct {
	// Generation is the caller's expected current generation of the host record.
	Generation int64 `json:"generation"`
	// FromStatus is the caller's expected current status. Required for server-side
	// transition validation without an extra DB read in the hot path.
	// Must be one of: ready, draining, drained, degraded (see legalTransitions).
	FromStatus string `json:"from_status"`
	// ReasonCode is a machine-readable code describing why the host is degraded.
	// Should be one of the ReasonXxx constants (e.g., AGENT_UNRESPONSIVE).
	ReasonCode string `json:"reason_code"`
}

// markDegradedResponse is returned by POST .../degraded.
type markDegradedResponse struct {
	HostID     string  `json:"host_id"`
	Status     string  `json:"status"`
	Generation int64   `json:"generation"`
	ReasonCode *string `json:"reason_code,omitempty"`
}

// markUnhealthyRequest is the payload for POST /internal/v1/hosts/{host_id}/unhealthy.
type markUnhealthyRequest struct {
	// Generation is the caller's expected current generation of the host record.
	Generation int64 `json:"generation"`
	// FromStatus is the caller's expected current status.
	// Must be one of: ready, draining, degraded (see legalTransitions).
	FromStatus string `json:"from_status"`
	// ReasonCode is a machine-readable code describing the failure.
	// Ambiguous-failure codes (AGENT_UNRESPONSIVE, HYPERVISOR_FAILED,
	// NETWORK_UNREACHABLE) will set fence_required=TRUE in the DB.
	ReasonCode string `json:"reason_code"`
}

// markUnhealthyResponse is returned by POST .../unhealthy.
type markUnhealthyResponse struct {
	HostID        string  `json:"host_id"`
	Status        string  `json:"status"`
	Generation    int64   `json:"generation"`
	ReasonCode    *string `json:"reason_code,omitempty"`
	FenceRequired bool    `json:"fence_required"`
}

// fenceRequiredListResponse is returned by GET /internal/v1/hosts/fence-required.
type fenceRequiredListResponse struct {
	Hosts []fenceRequiredEntry `json:"hosts"`
	Count int                  `json:"count"`
}

type fenceRequiredEntry struct {
	HostID     string  `json:"host_id"`
	Status     string  `json:"status"`
	Generation int64   `json:"generation"`
	ReasonCode *string `json:"reason_code,omitempty"`
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

// handleMarkDegraded handles POST /internal/v1/hosts/{host_id}/degraded.
//
// Transitions the host to 'degraded' with a reason code.
// The caller must supply generation (for CAS), from_status (for transition
// validation), and reason_code.
//
// Error mapping:
//   - 400: invalid JSON or missing required fields.
//   - 404: host not found (after CAS failure with valid generation).
//   - 409: generation mismatch or fromStatus mismatch.
//   - 422: illegal state transition (e.g., drained → degraded not in legalTransitions).
//   - 200: host transitioned to 'degraded'.
//   - 500: unexpected DB error.
//
// Source: vm-13-03__blueprint__ §implementation_decisions
//         "Introduce a DEGRADED state to precede the terminal UNHEALTHY state".
func (s *server) handleMarkDegraded(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostID := extractPathSegment(r.URL.Path, "hosts", "degraded")
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
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

	updated, err := s.inventory.MarkDegraded(r.Context(), hostID, req.Generation, req.FromStatus, req.ReasonCode)
	if err != nil {
		// ErrIllegalHostTransition → 422 Unprocessable Entity
		if isIllegalTransitionErr(err) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.log.Error("MarkDegraded failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "mark-degraded failed: "+err.Error())
		return
	}

	if !updated {
		// CAS failed: check whether host exists.
		host, lookupErr := s.inventory.repo.GetHostByID(r.Context(), hostID)
		if lookupErr != nil || host == nil {
			writeError(w, http.StatusNotFound, "host not found: "+hostID)
			return
		}
		writeError(w, http.StatusConflict, "generation mismatch or host not in expected status")
		return
	}

	// Re-read for the post-transition generation and reason_code.
	host, err := s.inventory.repo.GetHostByID(r.Context(), hostID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "status lookup failed after mark-degraded")
		return
	}

	s.log.Info("host marked degraded",
		"host_id", hostID,
		"reason_code", req.ReasonCode,
		"generation", host.Generation,
	)

	writeJSON(w, http.StatusOK, markDegradedResponse{
		HostID:     host.ID,
		Status:     host.Status,
		Generation: host.Generation,
		ReasonCode: host.ReasonCode,
	})
}

// handleMarkUnhealthy handles POST /internal/v1/hosts/{host_id}/unhealthy.
//
// Transitions the host to 'unhealthy' with a reason code.
// For ambiguous failure reason codes (AGENT_UNRESPONSIVE, HYPERVISOR_FAILED,
// NETWORK_UNREACHABLE), fence_required is set to TRUE in the DB.
//
// The fence_required flag is the fencing groundwork seam: it signals that
// a fencing controller must isolate this host before recovery automation
// may proceed. No actual fencing is implemented in this slice.
//
// Error mapping:
//   - 400: invalid JSON or missing required fields.
//   - 404: host not found.
//   - 409: generation mismatch or fromStatus mismatch.
//   - 422: illegal state transition.
//   - 200: host transitioned to 'unhealthy'.
//   - 500: unexpected DB error.
//
// Source: vm-13-03__blueprint__ §"Fencing Decision Logic",
//         §"Fencing Controller" (fence_required is the Slice 4 seam).
func (s *server) handleMarkUnhealthy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostID := extractPathSegment(r.URL.Path, "hosts", "unhealthy")
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
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

	fenceRequired, updated, err := s.inventory.MarkUnhealthy(r.Context(), hostID, req.Generation, req.FromStatus, req.ReasonCode)
	if err != nil {
		if isIllegalTransitionErr(err) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s.log.Error("MarkUnhealthy failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "mark-unhealthy failed: "+err.Error())
		return
	}

	if !updated {
		host, lookupErr := s.inventory.repo.GetHostByID(r.Context(), hostID)
		if lookupErr != nil || host == nil {
			writeError(w, http.StatusNotFound, "host not found: "+hostID)
			return
		}
		writeError(w, http.StatusConflict, "generation mismatch or host not in expected status")
		return
	}

	// Re-read for post-transition state.
	host, err := s.inventory.repo.GetHostByID(r.Context(), hostID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "status lookup failed after mark-unhealthy")
		return
	}

	s.log.Info("host marked unhealthy",
		"host_id", hostID,
		"reason_code", req.ReasonCode,
		"fence_required", fenceRequired,
		"generation", host.Generation,
	)

	writeJSON(w, http.StatusOK, markUnhealthyResponse{
		HostID:        host.ID,
		Status:        host.Status,
		Generation:    host.Generation,
		ReasonCode:    host.ReasonCode,
		FenceRequired: host.FenceRequired,
	})
}

// handleGetFenceRequired handles GET /internal/v1/hosts/fence-required.
//
// Returns all hosts with fence_required=TRUE. This is the observable surface
// for the fencing groundwork seam. An empty list means no hosts are waiting
// for fencing. A non-empty list means recovery automation must not proceed
// for those hosts until a fencing controller acts.
//
// This is a pure read endpoint — no state mutations.
// Status 200 always; empty list when no hosts need fencing.
//
// Source: vm-13-03__blueprint__ §"Fencing Controller" (Slice 4+ seam).
func (s *server) handleGetFenceRequired(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hosts, err := s.inventory.GetFenceRequiredHosts(r.Context())
	if err != nil {
		s.log.Error("GetFenceRequiredHosts failed", "error", err)
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
}

// handleGetHostStatus handles GET /internal/v1/hosts/{host_id}/status.
//
// Returns the current lifecycle state of a host for observability.
// VM-P2E Slice 2: now returns real generation and drain_reason values.
// VM-P2E Slice 3: now returns reason_code and fence_required.
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
		HostID:        host.ID,
		Status:        host.Status,
		Generation:    host.Generation,
		DrainReason:   host.DrainReason,
		ReasonCode:    host.ReasonCode,
		FenceRequired: host.FenceRequired,
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// isIllegalTransitionErr returns true when err wraps or equals ErrIllegalHostTransition.
func isIllegalTransitionErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), db.ErrIllegalHostTransition.Error())
}
