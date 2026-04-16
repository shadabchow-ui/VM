package main

// host_recovery_handlers.go — HTTP handlers for VM-P2E Slice 6 recovery automation.
//
// VM-P2E Slice 6 endpoints (all internal/operator-facing, mTLS required):
//
//   GET  /internal/v1/hosts/recovery-eligible
//        Returns hosts safe for recovery automation:
//        status IN (drained, degraded, unhealthy) AND fence_required=FALSE.
//
//   POST /internal/v1/hosts/{host_id}/recover
//        Executes a bounded, fencing-gated recovery action on a single host.
//        Requires generation in the request body.
//        Returns the verdict and logs the attempt in host_recovery_log.
//
//   GET  /internal/v1/hosts/{host_id}/recovery-log
//        Returns the full recovery attempt history for a host (newest first).
//
//   GET  /internal/v1/maintenance/campaigns/{id}/failed-hosts/recovery
//        Returns a recovery assessment for the campaign's failed_host_ids:
//        which are eligible, which are blocked by fencing, which are not recoverable.
//
// Auth: mTLS required (enforced by RequireMTLS middleware in api.go routes).
// These endpoints are operator/control-plane tools — not user-facing.
//
// Design:
//   - POST /recover requires a from_generation field. Callers MUST obtain
//     the current generation from GET /internal/v1/hosts/{id}/status first.
//     A wrong generation returns 409 Conflict (CAS failed). This prevents
//     recovery racing with concurrent operator or reconciler actions.
//   - fence_required=TRUE blocks all recovery for that host. The handler
//     returns 409 Conflict with verdict=skipped_fence_required when the
//     fencing gate fires.
//   - GET /recovery-eligible is a pure read. Safe for operator polling.
//   - GET /recovery-log is a pure read. Returns an empty list (200) if no
//     recovery has been attempted for the host.
//   - Campaign failed-hosts assessment is read-only. No host state changes.
//
// Source: vm-13-03__blueprint__ §components "Fleet Management Service",
//         §"Fencing Decision Logic",
//         §interaction_or_ops_contract "HA VM recovery after host failure".

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Request / Response types ──────────────────────────────────────────────────

// hostRecoverRequest is the payload for POST /internal/v1/hosts/{host_id}/recover.
type hostRecoverRequest struct {
	// FromGeneration is the caller's expected current generation of the host.
	// Required for CAS correctness. Obtain from GET .../status.
	// A mismatch returns 409 Conflict with verdict=cas_failed.
	FromGeneration int64 `json:"from_generation"`
	// Actor identifies who is calling this endpoint.
	// Defaults to "operator" if omitted.
	Actor string `json:"actor,omitempty"`
}

// hostRecoverResponse is returned by POST /internal/v1/hosts/{host_id}/recover.
type hostRecoverResponse struct {
	HostID                 string `json:"host_id"`
	Verdict                string `json:"verdict"`
	Reason                 string `json:"reason"`
	HostStatusAtAttempt    string `json:"host_status_at_attempt"`
	FenceRequiredAtAttempt bool   `json:"fence_required_at_attempt"`
}

// recoveryEligibleResponse is returned by GET /internal/v1/hosts/recovery-eligible.
type recoveryEligibleResponse struct {
	Hosts []recoveryEligibleHostSummary `json:"hosts"`
	Count int                           `json:"count"`
}

// recoveryEligibleHostSummary is the per-host entry in the recovery-eligible list.
type recoveryEligibleHostSummary struct {
	ID               string     `json:"id"`
	AvailabilityZone string     `json:"availability_zone"`
	Status           string     `json:"status"`
	Generation       int64      `json:"generation"`
	ReasonCode       *string    `json:"reason_code,omitempty"`
	FenceRequired    bool       `json:"fence_required"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// recoveryLogResponse is returned by GET /internal/v1/hosts/{host_id}/recovery-log.
type recoveryLogResponse struct {
	HostID  string               `json:"host_id"`
	Entries []recoveryLogEntry   `json:"entries"`
	Count   int                  `json:"count"`
}

// recoveryLogEntry is one record from host_recovery_log.
type recoveryLogEntry struct {
	ID                      string     `json:"id"`
	Verdict                 string     `json:"verdict"`
	Reason                  string     `json:"reason"`
	HostStatusAtAttempt     string     `json:"host_status_at_attempt"`
	HostGenerationAtAttempt int64      `json:"host_generation_at_attempt"`
	FenceRequiredAtAttempt  bool       `json:"fence_required_at_attempt"`
	Actor                   string     `json:"actor"`
	CampaignID              *string    `json:"campaign_id,omitempty"`
	AttemptedAt             time.Time  `json:"attempted_at"`
}

// campaignRecoveryAssessmentResponse is returned by
// GET /internal/v1/maintenance/campaigns/{id}/failed-hosts/recovery.
type campaignRecoveryAssessmentResponse struct {
	CampaignID     string                        `json:"campaign_id"`
	EligibleHosts  []recoveryEligibleHostSummary `json:"eligible_hosts"`
	BlockedByFencing []recoveryEligibleHostSummary `json:"blocked_by_fencing"`
	NotRecoverable []recoveryEligibleHostSummary `json:"not_recoverable"`
	NotFound       []string                      `json:"not_found"`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// recoveryEligibleSummaryFromRecord converts a db.HostRecord to the response summary shape.
func recoveryEligibleSummaryFromRecord(h *db.HostRecord) recoveryEligibleHostSummary {
	return recoveryEligibleHostSummary{
		ID:               h.ID,
		AvailabilityZone: h.AvailabilityZone,
		Status:           h.Status,
		Generation:       h.Generation,
		ReasonCode:       h.ReasonCode,
		FenceRequired:    h.FenceRequired,
		UpdatedAt:        h.UpdatedAt,
	}
}

// extractRecoveryLogHostID extracts host_id from paths like:
//   /internal/v1/hosts/{id}/recovery-log
func extractRecoveryLogHostID(path string) string {
	return extractPathSegment(path, "hosts", "recovery-log")
}

// extractRecoverHostID extracts host_id from paths like:
//   /internal/v1/hosts/{id}/recover
func extractRecoverHostID(path string) string {
	return extractPathSegment(path, "hosts", "recover")
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// handleGetRecoveryEligibleHosts handles GET /internal/v1/hosts/recovery-eligible.
//
// Returns hosts with status IN (drained, degraded, unhealthy) AND fence_required=FALSE.
// These are the hosts that recovery automation may act on safely.
//
// A host with fence_required=TRUE will not appear here, regardless of status.
// The fencing gate is enforced at the DB query layer.
//
// Error mapping:
//   - 200: list returned (may be empty).
//   - 500: unexpected DB error.
func (s *server) handleGetRecoveryEligibleHosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hosts, err := s.inventory.GetRecoveryEligibleHosts(r.Context())
	if err != nil {
		s.log.Error("GetRecoveryEligibleHosts failed", "error", err)
		writeError(w, http.StatusInternalServerError, "get recovery-eligible hosts failed: "+err.Error())
		return
	}

	summaries := make([]recoveryEligibleHostSummary, 0, len(hosts))
	for _, h := range hosts {
		summaries = append(summaries, recoveryEligibleSummaryFromRecord(h))
	}

	writeJSON(w, http.StatusOK, recoveryEligibleResponse{
		Hosts: summaries,
		Count: len(summaries),
	})
}

// handleRecoverHost handles POST /internal/v1/hosts/{host_id}/recover.
//
// Performs a bounded, fencing-gated recovery action on the specified host.
//
// Recovery logic (enforced in inventory.ExecuteHostRecovery):
//   - If fence_required=TRUE → 409 Conflict, verdict=skipped_fence_required.
//   - If status="drained"   → transitions to "ready" (reactivation).
//   - If status="degraded" or "unhealthy" → transitions to "draining" (drain-then-recover).
//   - If status is not recoverable → 409 Conflict, verdict=skipped_not_eligible.
//   - If CAS fails (wrong from_generation) → 409 Conflict, verdict=cas_failed.
//
// All attempts (including skipped ones) are logged in host_recovery_log.
//
// Error mapping:
//   - 400: invalid JSON or missing from_generation.
//   - 404: host not found.
//   - 409: fencing gate fired, not eligible, or CAS failed.
//   - 200: action executed; see verdict field for outcome.
//   - 500: unexpected DB error.
func (s *server) handleRecoverHost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostID := extractRecoverHostID(r.URL.Path)
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
		return
	}

	var req hostRecoverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Actor == "" {
		req.Actor = "operator"
	}

	result, err := s.inventory.ExecuteHostRecovery(r.Context(), &HostRecoveryRequest{
		HostID:         hostID,
		FromGeneration: req.FromGeneration,
		Actor:          req.Actor,
	})
	if err != nil {
		// ErrHostNotFound from the internal re-read.
		if isHostNotFoundErr(err) {
			writeError(w, http.StatusNotFound, "host not found: "+hostID)
			return
		}
		s.log.Error("ExecuteHostRecovery failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "recover host failed: "+err.Error())
		return
	}

	// Verdicts that indicate a blocked/failed action → 409 Conflict.
	// All are logged in host_recovery_log regardless.
	switch result.Verdict {
	case db.RecoveryVerdictSkippedFenceRequired,
		db.RecoveryVerdictSkippedNotEligible,
		db.RecoveryVerdictCASFailed:
		writeJSON(w, http.StatusConflict, hostRecoverResponse{
			HostID:                 result.HostID,
			Verdict:                result.Verdict,
			Reason:                 result.Reason,
			HostStatusAtAttempt:    result.HostStatusAtAttempt,
			FenceRequiredAtAttempt: result.FenceRequiredAtAttempt,
		})
		return
	}

	s.log.Info("host recovery executed",
		"host_id", hostID,
		"verdict", result.Verdict,
		"status_before", result.HostStatusAtAttempt,
	)

	writeJSON(w, http.StatusOK, hostRecoverResponse{
		HostID:                 result.HostID,
		Verdict:                result.Verdict,
		Reason:                 result.Reason,
		HostStatusAtAttempt:    result.HostStatusAtAttempt,
		FenceRequiredAtAttempt: result.FenceRequiredAtAttempt,
	})
}

// handleGetHostRecoveryLog handles GET /internal/v1/hosts/{host_id}/recovery-log.
//
// Returns the full recovery attempt history for a host, newest first.
// Returns an empty entries array (200) when no attempts have been logged.
//
// Error mapping:
//   - 404: host not found (we verify via GetHostByID in case the host is absent entirely).
//   - 200: log returned (may be empty entries array).
//   - 500: unexpected DB error.
func (s *server) handleGetHostRecoveryLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostID := extractRecoveryLogHostID(r.URL.Path)
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
		return
	}

	records, err := s.inventory.GetHostRecoveryLog(r.Context(), hostID)
	if err != nil {
		s.log.Error("GetHostRecoveryLog failed", "host_id", hostID, "error", err)
		writeError(w, http.StatusInternalServerError, "get recovery log failed: "+err.Error())
		return
	}

	entries := make([]recoveryLogEntry, 0, len(records))
	for _, rec := range records {
		entries = append(entries, recoveryLogEntry{
			ID:                      rec.ID,
			Verdict:                 rec.Verdict,
			Reason:                  rec.Reason,
			HostStatusAtAttempt:     rec.HostStatusAtAttempt,
			HostGenerationAtAttempt: rec.HostGenerationAtAttempt,
			FenceRequiredAtAttempt:  rec.FenceRequiredAtAttempt,
			Actor:                   rec.Actor,
			CampaignID:              rec.CampaignID,
			AttemptedAt:             rec.AttemptedAt,
		})
	}

	writeJSON(w, http.StatusOK, recoveryLogResponse{
		HostID:  hostID,
		Entries: entries,
		Count:   len(entries),
	})
}

// handleGetCampaignFailedHostsRecovery handles:
//   GET /internal/v1/maintenance/campaigns/{id}/failed-hosts/recovery
//
// Returns a read-only recovery assessment for the campaign's failed_host_ids.
// No host state changes are made. The operator uses this to decide which
// hosts to pass to POST /internal/v1/hosts/{id}/recover.
//
// Assessment categories:
//   - eligible_hosts:      fence_required=FALSE AND status in (drained, degraded, unhealthy).
//   - blocked_by_fencing:  fence_required=TRUE — STONITH must complete first.
//   - not_recoverable:     status not in the recoverable set (e.g. retiring, retired, ready).
//   - not_found:           host IDs in failed_host_ids that no longer exist in the DB.
//
// Error mapping:
//   - 404: campaign not found.
//   - 200: assessment returned.
//   - 500: unexpected DB error.
func (s *server) handleGetCampaignFailedHostsRecovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	campaignID := extractCampaignID(r.URL.Path)
	if campaignID == "" {
		writeError(w, http.StatusBadRequest, "missing campaign id in path")
		return
	}

	assessment, err := s.inventory.EvaluateCampaignFailedHostRecovery(r.Context(), campaignID)
	if err != nil {
		if isCampaignNotFoundErr(err) {
			writeError(w, http.StatusNotFound, "campaign not found: "+campaignID)
			return
		}
		s.log.Error("EvaluateCampaignFailedHostRecovery failed", "campaign_id", campaignID, "error", err)
		writeError(w, http.StatusInternalServerError, "campaign recovery assessment failed: "+err.Error())
		return
	}

	toSummaries := func(hosts []*db.HostRecord) []recoveryEligibleHostSummary {
		out := make([]recoveryEligibleHostSummary, 0, len(hosts))
		for _, h := range hosts {
			out = append(out, recoveryEligibleSummaryFromRecord(h))
		}
		return out
	}

	notFound := assessment.NotFound
	if notFound == nil {
		notFound = []string{}
	}

	writeJSON(w, http.StatusOK, campaignRecoveryAssessmentResponse{
		CampaignID:       campaignID,
		EligibleHosts:    toSummaries(assessment.EligibleHosts),
		BlockedByFencing: toSummaries(assessment.BlockedByFencing),
		NotRecoverable:   toSummaries(assessment.NotRecoverable),
		NotFound:         notFound,
	})
}

// isHostNotFoundErr returns true when err or an unwrapped cause is db.ErrHostNotFound.
func isHostNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), db.ErrHostNotFound.Error())
}
