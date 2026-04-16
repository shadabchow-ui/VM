package main

// host_maintenance_handlers.go — HTTP handlers for the maintenance campaign API.
//
// VM-P2E Slice 5: maintenance orchestration foundation.
//
// Endpoints:
//   POST /internal/v1/maintenance/campaigns              — create a campaign
//   GET  /internal/v1/maintenance/campaigns              — list campaigns (filterable by status)
//   GET  /internal/v1/maintenance/campaigns/{id}         — get campaign by ID
//   POST /internal/v1/maintenance/campaigns/{id}/advance — advance: drain next batch
//   POST /internal/v1/maintenance/campaigns/{id}/pause   — pause a running campaign
//   POST /internal/v1/maintenance/campaigns/{id}/resume  — resume a paused campaign
//   POST /internal/v1/maintenance/campaigns/{id}/cancel  — cancel a campaign
//
// Auth: mTLS required (same as host lifecycle endpoints).
//
// Design:
//   - All campaign mutations produce a campaignResponse reflecting current state.
//   - Blast-radius limit (db.MaxCampaignParallel) is enforced at CreateCampaign.
//     Attempts to set max_parallel above the limit return 400.
//   - Advance is idempotent across retries: calling advance when a campaign is
//     already running does not duplicate host drains — NextHosts skips hosts
//     already in completed_host_ids or failed_host_ids.
//   - Campaign advance uses generation=0 for DrainHost calls (see inventory.go).
//     Callers needing generation-exact drain must use the direct drain endpoint.
//   - Slice 6 forward seam: failed_host_ids in the campaign response is the
//     observable list for future recovery automation.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator",
//         §interaction_or_ops_contract "Operator initiates a fleet-wide kernel update".

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Request / Response types ──────────────────────────────────────────────────

// createCampaignRequest is the payload for POST /internal/v1/maintenance/campaigns.
type createCampaignRequest struct {
	// ID is the caller-generated unique campaign ID. Recommend UUID4.
	// Required. The DB enforces uniqueness.
	ID string `json:"id"`
	// Reason is a human-readable label for the campaign (e.g. "kernel-4.19 patch").
	// Required for audit trail.
	Reason string `json:"reason"`
	// TargetHostIDs is the ordered list of host IDs to include in the campaign.
	// Required; must be non-empty.
	TargetHostIDs []string `json:"target_host_ids"`
	// MaxParallel is the blast-radius limit: maximum number of hosts to drain
	// concurrently per advance call. Must be between 1 and MaxCampaignParallel.
	// Defaults to 1 if omitted (safest: one host at a time).
	MaxParallel int `json:"max_parallel,omitempty"`
}

// campaignResponse is the canonical response shape for all campaign endpoints.
//
// Slice 6 forward seam:
//   - failed_host_ids is explicitly included so recovery automation can observe
//     which hosts failed without a schema change.
//   - campaign_status values are stable: pending, running, paused, completed, cancelled.
type campaignResponse struct {
	ID               string    `json:"id"`
	Reason           string    `json:"reason"`
	Status           string    `json:"status"`
	MaxParallel      int       `json:"max_parallel"`
	TargetHostIDs    []string  `json:"target_host_ids"`
	CompletedHostIDs []string  `json:"completed_host_ids"`
	FailedHostIDs    []string  `json:"failed_host_ids"`
	// RemainingCount is the number of target hosts not yet completed or failed.
	RemainingCount int `json:"remaining_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// campaignListResponse is returned by GET /internal/v1/maintenance/campaigns.
type campaignListResponse struct {
	Campaigns []campaignResponse `json:"campaigns"`
	Count     int                `json:"count"`
}

// advanceCampaignRequest is the payload for POST .../advance.
type advanceCampaignRequest struct {
	// DrainReason is an optional human-readable reason forwarded to the
	// individual host drain calls. Stored as drain_reason on each host record.
	DrainReason string `json:"drain_reason,omitempty"`
}

// advanceCampaignResponse is returned by POST .../advance.
type advanceCampaignResponse struct {
	CampaignID string                 `json:"campaign_id"`
	Status     string                 `json:"status"`
	Actioned   []advanceHostOutcome   `json:"actioned"`
	// RemainingCount is the number of hosts still to be actioned after this advance.
	RemainingCount int `json:"remaining_count"`
}

// advanceHostOutcome records what happened to one host during an advance.
type advanceHostOutcome struct {
	HostID  string `json:"host_id"`
	Outcome string `json:"outcome"` // "completed" | "failed"
	// Error is non-empty when outcome="failed".
	// Slice 6 note: a recovery actor may read this to decide next action.
	Error string `json:"error,omitempty"`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// campaignFromRecord converts a db.CampaignRecord to the HTTP response shape.
func campaignFromRecord(c *db.CampaignRecord) campaignResponse {
	completed := c.CompletedHostIDs
	if completed == nil {
		completed = []string{}
	}
	failed := c.FailedHostIDs
	if failed == nil {
		failed = []string{}
	}
	targets := c.TargetHostIDs
	if targets == nil {
		targets = []string{}
	}
	return campaignResponse{
		ID:               c.ID,
		Reason:           c.CampaignReason,
		Status:           c.Status,
		MaxParallel:      c.MaxParallel,
		TargetHostIDs:    targets,
		CompletedHostIDs: completed,
		FailedHostIDs:    failed,
		RemainingCount:   len(targets) - len(completed) - len(failed),
		CreatedAt:        c.CreatedAt,
		UpdatedAt:        c.UpdatedAt,
	}
}

// extractCampaignID extracts the campaign ID from paths like:
//   /internal/v1/maintenance/campaigns/{id}
//   /internal/v1/maintenance/campaigns/{id}/advance
func extractCampaignID(path string) string {
	const prefix = "/internal/v1/maintenance/campaigns/"
	idx := strings.Index(path, prefix)
	if idx < 0 {
		return ""
	}
	rest := path[idx+len(prefix):]
	// rest may be "{id}" or "{id}/advance" etc. — take the first segment.
	if slash := strings.Index(rest, "/"); slash >= 0 {
		return rest[:slash]
	}
	return rest
}

// isCampaignNotFoundErr returns true when err wraps ErrCampaignNotFound.
func isCampaignNotFoundErr(err error) bool {
	return err != nil && errors.Is(err, db.ErrCampaignNotFound)
}

// isBlastRadiusErr returns true when err wraps ErrBlastRadiusExceeded.
func isBlastRadiusErr(err error) bool {
	return err != nil && errors.Is(err, db.ErrBlastRadiusExceeded)
}

// isNoTargetsErr returns true when err wraps ErrCampaignNoTargets.
func isNoTargetsErr(err error) bool {
	return err != nil && errors.Is(err, db.ErrCampaignNoTargets)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// handleCreateCampaign handles POST /internal/v1/maintenance/campaigns.
//
// Creates a new maintenance campaign with a target host list and blast-radius
// limit. The campaign starts in 'pending' status.
//
// Error mapping:
//   - 400: invalid JSON, missing required fields, max_parallel exceeds blast limit,
//          or empty target_host_ids.
//   - 201: campaign created successfully.
//   - 500: unexpected DB error.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (s *server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req createCampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if len(req.TargetHostIDs) == 0 {
		writeError(w, http.StatusBadRequest, "target_host_ids must be non-empty")
		return
	}
	if req.MaxParallel == 0 {
		req.MaxParallel = 1 // default: one host at a time
	}

	campaign, err := s.inventory.CreateCampaign(r.Context(), req.ID, req.Reason, req.TargetHostIDs, req.MaxParallel)
	if err != nil {
		if isBlastRadiusErr(err) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if isNoTargetsErr(err) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Error("CreateCampaign failed", "campaign_id", req.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "create campaign failed: "+err.Error())
		return
	}

	s.log.Info("maintenance campaign created",
		"campaign_id", campaign.ID,
		"target_count", len(campaign.TargetHostIDs),
		"max_parallel", campaign.MaxParallel,
	)

	writeJSON(w, http.StatusCreated, campaignFromRecord(campaign))
}

// handleGetCampaign handles GET /internal/v1/maintenance/campaigns/{id}.
//
// Returns the current state of a campaign including completed and failed host lists.
// This is the primary observability surface for operator tooling.
//
// Slice 6 seam: failed_host_ids in the response is stable and observable
// without a schema change — recovery automation reads this list.
//
// Error mapping:
//   - 404: campaign not found.
//   - 200: campaign returned.
//   - 500: unexpected DB error.
func (s *server) handleGetCampaign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := extractCampaignID(r.URL.Path)
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing campaign id in path")
		return
	}

	campaign, err := s.inventory.GetCampaign(r.Context(), id)
	if err != nil {
		if isCampaignNotFoundErr(err) {
			writeError(w, http.StatusNotFound, "campaign not found: "+id)
			return
		}
		s.log.Error("GetCampaign failed", "campaign_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "get campaign failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, campaignFromRecord(campaign))
}

// handleListCampaigns handles GET /internal/v1/maintenance/campaigns.
//
// Returns all campaigns, optionally filtered by status query param.
// Example: GET /internal/v1/maintenance/campaigns?status=running,paused
//
// Status 200 always; empty list when no campaigns match.
func (s *server) handleListCampaigns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var statuses []string
	if sv := r.URL.Query().Get("status"); sv != "" {
		for _, s := range strings.Split(sv, ",") {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				statuses = append(statuses, trimmed)
			}
		}
	}

	campaigns, err := s.inventory.ListCampaigns(r.Context(), statuses)
	if err != nil {
		s.log.Error("ListCampaigns failed", "error", err)
		writeError(w, http.StatusInternalServerError, "list campaigns failed: "+err.Error())
		return
	}

	entries := make([]campaignResponse, 0, len(campaigns))
	for _, c := range campaigns {
		entries = append(entries, campaignFromRecord(c))
	}

	writeJSON(w, http.StatusOK, campaignListResponse{
		Campaigns: entries,
		Count:     len(entries),
	})
}

// handleAdvanceCampaign handles POST /internal/v1/maintenance/campaigns/{id}/advance.
//
// Drains the next batch of hosts in the campaign, up to max_parallel hosts.
// The blast-radius limit (max_parallel) was locked at campaign creation time and
// is enforced here by NextHosts(campaign.MaxParallel).
//
// Advance is safe to retry: hosts already in completed_host_ids or failed_host_ids
// are skipped. Idempotency note: generation=0 is used for DrainHost calls —
// if a host was modified concurrently, the CAS fails and the host is recorded as
// "failed" in this campaign. The operator can then use the single-host drain
// endpoint with the correct generation to drain that specific host.
//
// Error mapping:
//   - 400: invalid JSON.
//   - 404: campaign not found.
//   - 409: campaign is terminal or paused (cannot advance).
//   - 200: advance completed; actioned list shows per-host outcomes.
//   - 500: unexpected DB error.
//
// Source: vm-13-03__blueprint__ §interaction_or_ops_contract
//         "Operator initiates a fleet-wide kernel update".
func (s *server) handleAdvanceCampaign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := extractCampaignID(r.URL.Path)
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing campaign id in path")
		return
	}

	var req advanceCampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	result, err := s.inventory.AdvanceCampaign(r.Context(), id, req.DrainReason)
	if err != nil {
		if isCampaignNotFoundErr(err) {
			writeError(w, http.StatusNotFound, "campaign not found: "+id)
			return
		}
		// Terminal or paused campaigns cannot be advanced.
		if strings.Contains(err.Error(), "terminal") || strings.Contains(err.Error(), "paused") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.log.Error("AdvanceCampaign failed", "campaign_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "advance campaign failed: "+err.Error())
		return
	}

	// Re-read for accurate remaining count after the advance.
	campaign, readErr := s.inventory.GetCampaign(r.Context(), id)
	var remainingCount int
	if readErr == nil && campaign != nil {
		remainingCount = len(campaign.TargetHostIDs) - len(campaign.CompletedHostIDs) - len(campaign.FailedHostIDs)
	}

	s.log.Info("campaign advanced",
		"campaign_id", id,
		"actioned", len(result.Actioned),
		"status", result.Status,
	)

	outcomes := make([]advanceHostOutcome, 0, len(result.Actioned))
	for _, a := range result.Actioned {
		outcomes = append(outcomes, advanceHostOutcome{
			HostID:  a.HostID,
			Outcome: a.Outcome,
			Error:   a.Error,
		})
	}

	writeJSON(w, http.StatusOK, advanceCampaignResponse{
		CampaignID:     id,
		Status:         result.Status,
		Actioned:       outcomes,
		RemainingCount: remainingCount,
	})
}

// handlePauseCampaign handles POST /internal/v1/maintenance/campaigns/{id}/pause.
//
// Halts further advancement of a running campaign. In-flight drains already
// started on individual hosts will complete naturally (host lifecycle is
// independent of campaign status). Call /resume to continue.
//
// Error mapping:
//   - 404: campaign not found.
//   - 409: campaign already paused or terminal.
//   - 200: campaign paused.
//   - 500: unexpected DB error.
func (s *server) handlePauseCampaign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := extractCampaignID(r.URL.Path)
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing campaign id in path")
		return
	}

	updated, err := s.inventory.PauseCampaign(r.Context(), id)
	if err != nil {
		s.log.Error("PauseCampaign failed", "campaign_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "pause campaign failed: "+err.Error())
		return
	}

	if !updated {
		// Either already paused or terminal. Distinguish via re-read.
		campaign, lookupErr := s.inventory.GetCampaign(r.Context(), id)
		if lookupErr != nil && isCampaignNotFoundErr(lookupErr) {
			writeError(w, http.StatusNotFound, "campaign not found: "+id)
			return
		}
		if campaign != nil && campaign.IsTerminal() {
			writeError(w, http.StatusConflict, "campaign is already terminal (status="+campaign.Status+")")
			return
		}
		writeError(w, http.StatusConflict, "campaign is already paused or cannot be paused from current status")
		return
	}

	campaign, err := s.inventory.GetCampaign(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "status lookup failed after pause")
		return
	}

	s.log.Info("campaign paused", "campaign_id", id)
	writeJSON(w, http.StatusOK, campaignFromRecord(campaign))
}

// handleResumeCampaign handles POST /internal/v1/maintenance/campaigns/{id}/resume.
//
// Resumes a paused campaign. The next call to /advance will proceed.
// Does not automatically advance — the operator must call /advance explicitly.
//
// Error mapping:
//   - 404: campaign not found.
//   - 409: campaign not paused (already running, completed, or cancelled).
//   - 200: campaign resumed (status=running).
//   - 500: unexpected DB error.
func (s *server) handleResumeCampaign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := extractCampaignID(r.URL.Path)
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing campaign id in path")
		return
	}

	updated, err := s.inventory.ResumeCampaign(r.Context(), id)
	if err != nil {
		s.log.Error("ResumeCampaign failed", "campaign_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "resume campaign failed: "+err.Error())
		return
	}

	if !updated {
		_, lookupErr := s.inventory.GetCampaign(r.Context(), id)
		if lookupErr != nil && isCampaignNotFoundErr(lookupErr) {
			writeError(w, http.StatusNotFound, "campaign not found: "+id)
			return
		}
		writeError(w, http.StatusConflict, "campaign is not in paused state")
		return
	}

	campaign, err := s.inventory.GetCampaign(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "status lookup failed after resume")
		return
	}

	s.log.Info("campaign resumed", "campaign_id", id)
	writeJSON(w, http.StatusOK, campaignFromRecord(campaign))
}

// handleCancelCampaign handles POST /internal/v1/maintenance/campaigns/{id}/cancel.
//
// Cancels a campaign. In-flight drains already started on individual hosts will
// complete naturally. A cancelled campaign cannot be restarted.
// The failed_host_ids list remains observable for Slice 6 recovery automation.
//
// Error mapping:
//   - 404: campaign not found.
//   - 409: campaign already in a terminal state.
//   - 200: campaign cancelled.
//   - 500: unexpected DB error.
func (s *server) handleCancelCampaign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := extractCampaignID(r.URL.Path)
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing campaign id in path")
		return
	}

	updated, err := s.inventory.CancelCampaign(r.Context(), id)
	if err != nil {
		s.log.Error("CancelCampaign failed", "campaign_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "cancel campaign failed: "+err.Error())
		return
	}

	if !updated {
		campaign, lookupErr := s.inventory.GetCampaign(r.Context(), id)
		if lookupErr != nil && isCampaignNotFoundErr(lookupErr) {
			writeError(w, http.StatusNotFound, "campaign not found: "+id)
			return
		}
		if campaign != nil && campaign.IsTerminal() {
			writeError(w, http.StatusConflict, "campaign is already in a terminal state (status="+campaign.Status+")")
			return
		}
		writeError(w, http.StatusConflict, "campaign could not be cancelled from current status")
		return
	}

	campaign, err := s.inventory.GetCampaign(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "status lookup failed after cancel")
		return
	}

	s.log.Info("campaign cancelled", "campaign_id", id)
	writeJSON(w, http.StatusOK, campaignFromRecord(campaign))
}
