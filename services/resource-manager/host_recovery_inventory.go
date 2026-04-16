package main

// host_recovery_inventory.go — HostInventory methods for VM-P2E Slice 6 recovery automation.
//
// VM-P2E Slice 6 additions:
//   - GetRecoveryEligibleHosts: reads the DB recovery-eligible host list.
//   - ExecuteHostRecovery: bounded single-host recovery with fencing gate.
//   - GetHostRecoveryLog: per-host recovery history for operator inspection.
//   - EvaluateCampaignFailedHostRecovery: campaign-scoped recovery assessment
//     for hosts in a campaign's failed_host_ids list.
//
// Design:
//   - ExecuteHostRecovery is the only method that writes state. All writes
//     go through the existing CAS-protected DB methods (UpdateHostStatus,
//     MarkHostDrained, etc.) — no new CAS mechanism is introduced.
//   - Fencing gate: ExecuteHostRecovery reads the host record and checks
//     fence_required before acting. If fence_required=TRUE, it logs a
//     RecoveryVerdictSkippedFenceRequired entry and returns without touching
//     host state. The DB query GetRecoveryEligibleHosts also enforces
//     fence_required=FALSE, but the per-host re-read here is an explicit
//     double-check because fence_required may have changed between the
//     list-query and the per-host call.
//   - Recovery actions are bounded:
//       "drained"   → UpdateHostStatus to "ready" (reactivation path).
//       "degraded"  → UpdateHostStatus to "draining" (drain-then-recover path).
//       "unhealthy" → UpdateHostStatus to "draining" (drain-then-recover path).
//     No other transitions are attempted.
//   - All decisions are persisted in host_recovery_log via InsertRecoveryLog.
//   - Campaign-scoped recovery (EvaluateCampaignFailedHostRecovery) is
//     assessment-only: it reads the campaign's failed_host_ids and evaluates
//     each host's eligibility. It does NOT automatically trigger recovery
//     actions — that remains an explicit per-host call to ExecuteHostRecovery.
//     This keeps automation bounded and operator-visible.
//
// Source: vm-13-03__skill__ §instructions "fence-then-recover" protocol,
//         vm-13-03__blueprint__ §"Fencing Controller",
//         §interaction_or_ops_contract "HA VM recovery after host failure".

import (
	"context"
	"fmt"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── Request/result types ──────────────────────────────────────────────────────

// HostRecoveryRequest is the input to ExecuteHostRecovery.
type HostRecoveryRequest struct {
	// HostID is the host to attempt recovery on.
	HostID string
	// FromGeneration is the caller's expected current generation of the host record.
	// Required for CAS correctness. Obtain from GET /internal/v1/hosts/{id}/status.
	// If the generation has changed since the caller read it, the CAS will fail
	// and RecoveryVerdictCASFailed will be returned.
	FromGeneration int64
	// Actor identifies what initiated the recovery attempt.
	// Use "operator" for direct API calls; "recovery_loop" for automated runs.
	Actor string
	// CampaignID is optional — set when recovery is triggered by a campaign's
	// failed_host_ids list. Pass nil for standalone host recovery.
	CampaignID *string
}

// HostRecoveryResult is the outcome of ExecuteHostRecovery.
type HostRecoveryResult struct {
	HostID  string
	Verdict string
	Reason  string
	// HostStatusAtAttempt is the host status that was observed before the action.
	HostStatusAtAttempt string
	// FenceRequiredAtAttempt is the fence_required value observed before the action.
	FenceRequiredAtAttempt bool
}

// CampaignRecoveryAssessment is the result of EvaluateCampaignFailedHostRecovery.
type CampaignRecoveryAssessment struct {
	CampaignID string
	// EligibleHosts are failed campaign hosts that have fence_required=FALSE
	// and a recoverable status. These can be passed to ExecuteHostRecovery.
	EligibleHosts []*db.HostRecord
	// BlockedByFencing are failed campaign hosts with fence_required=TRUE.
	// Recovery is blocked until STONITH completes.
	BlockedByFencing []*db.HostRecord
	// NotRecoverable are failed campaign hosts with a status that does not
	// support recovery (e.g. retiring, retired, ready).
	NotRecoverable []*db.HostRecord
	// NotFound are failed campaign host IDs that do not exist in the DB.
	NotFound []string
}

// ── HostInventory methods ─────────────────────────────────────────────────────

// GetRecoveryEligibleHosts returns hosts that are safe candidates for recovery.
//
// Eligibility criteria (enforced at the DB layer):
//   - status IN ('drained', 'degraded', 'unhealthy')
//   - fence_required = FALSE
//
// This list is the starting point for bounded recovery automation.
// Hosts that need fencing (fence_required=TRUE) are excluded at the DB query;
// they will not appear here regardless of their status.
//
// Source: vm-13-03__skill__ §instructions "fence-then-recover" protocol.
func (i *HostInventory) GetRecoveryEligibleHosts(ctx context.Context) ([]*db.HostRecord, error) {
	hosts, err := i.repo.GetRecoveryEligibleHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("GetRecoveryEligibleHosts: %w", err)
	}
	return hosts, nil
}

// GetHostRecoveryLog returns the recovery attempt history for a single host.
//
// Returns the full history, ordered most-recent-first.
// Returns an empty slice (not an error) when no attempts have been made.
//
// Source: vm-13-03__blueprint__ §"Fencing Controller" (operator inspection).
func (i *HostInventory) GetHostRecoveryLog(ctx context.Context, hostID string) ([]*db.RecoveryLogRecord, error) {
	records, err := i.repo.GetHostRecoveryLog(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("GetHostRecoveryLog: %w", err)
	}
	return records, nil
}

// ExecuteHostRecovery performs a bounded, fencing-gated recovery action on a
// single host and records the decision in host_recovery_log.
//
// Algorithm:
//  1. Read the current host record (re-read, not cached from list query).
//  2. If fence_required=TRUE → log RecoveryVerdictSkippedFenceRequired, return.
//  3. Determine action based on status:
//     - "drained"   → UpdateHostStatus to "ready" (reactivation).
//     - "degraded"  → UpdateHostStatus to "draining" (drain-then-recover).
//     - "unhealthy" → UpdateHostStatus to "draining" (drain-then-recover).
//     - anything else → log RecoveryVerdictSkippedNotEligible, return.
//  4. Attempt the CAS transition using req.FromGeneration.
//     - CAS failure → log RecoveryVerdictCASFailed, return (no panic, no retry).
//  5. Log the verdict.
//
// Concurrency safety:
//   - The fence_required re-read at step 1 is the explicit double-check.
//   - The generation CAS at step 4 prevents races with concurrent operators.
//   - If both the re-read and the CAS race, the CAS is the safety net.
//
// The caller is responsible for calling this with the correct generation.
// Use GET /internal/v1/hosts/{id}/status to obtain the current generation.
//
// Returns a HostRecoveryResult describing what happened.
// Returns error only for unexpected DB failures (not for CAS failure or skip).
//
// Source: vm-13-03__skill__ §instructions "fence-then-recover" protocol,
//         vm-13-03__blueprint__ §"HA VM Recovery" and §"Fencing Decision Logic".
func (i *HostInventory) ExecuteHostRecovery(ctx context.Context, req *HostRecoveryRequest) (*HostRecoveryResult, error) {
	// Step 1: re-read the host record for current state.
	host, err := i.repo.GetHostByID(ctx, req.HostID)
	if err != nil {
		return nil, fmt.Errorf("ExecuteHostRecovery GetHostByID: %w", err)
	}

	result := &HostRecoveryResult{
		HostID:                 req.HostID,
		HostStatusAtAttempt:    host.Status,
		FenceRequiredAtAttempt: host.FenceRequired,
	}

	actor := req.Actor
	if actor == "" {
		actor = "operator"
	}

	logRec := &db.RecoveryLogRecord{
		ID:                      idgen.New(idgen.PrefixEvent),
		HostID:                  req.HostID,
		HostStatusAtAttempt:     host.Status,
		HostGenerationAtAttempt: host.Generation,
		FenceRequiredAtAttempt:  host.FenceRequired,
		Actor:                   actor,
		CampaignID:              req.CampaignID,
	}

	// Step 2: hard fencing gate. If fence_required=TRUE we must not act.
	// The STONITH sequence must complete before recovery automation proceeds.
	// Source: vm-13-03__skill__ §instructions "Recovery of HA VMs must only
	//         commence after the Fencing Controller has successfully isolated
	//         the host and transitioned its state to FENCED."
	if host.FenceRequired {
		result.Verdict = db.RecoveryVerdictSkippedFenceRequired
		result.Reason = fmt.Sprintf(
			"host %s has fence_required=TRUE (status=%s, generation=%d); STONITH must complete first",
			req.HostID, host.Status, host.Generation,
		)
		logRec.Verdict = result.Verdict
		logRec.Reason = result.Reason
		_ = i.repo.InsertRecoveryLog(ctx, logRec)
		return result, nil
	}

	// Step 3: determine action based on status.
	var newStatus string
	var verdictOnSuccess string
	var reasonOnSuccess string

	switch host.Status {
	case "drained":
		// Reactivation: drained → ready.
		// The host's workload is empty (it was drained) so it can accept new VMs.
		newStatus = "ready"
		verdictOnSuccess = db.RecoveryVerdictReactivated
		reasonOnSuccess = fmt.Sprintf(
			"host %s reactivated from drained state (generation=%d)",
			req.HostID, host.Generation,
		)
	case "degraded", "unhealthy":
		// Drain-then-recover path: → draining.
		// The host has potentially active VMs; drain them before reactivating.
		// Once drained, the operator can call recover again to reactivate.
		newStatus = "draining"
		verdictOnSuccess = db.RecoveryVerdictDrainInitiated
		reasonOnSuccess = fmt.Sprintf(
			"host %s (status=%s, generation=%d) transitioned to draining for drain-then-recover",
			req.HostID, host.Status, host.Generation,
		)
	default:
		// Not eligible for automated recovery.
		result.Verdict = db.RecoveryVerdictSkippedNotEligible
		result.Reason = fmt.Sprintf(
			"host %s has status=%q which is not recoverable by automation",
			req.HostID, host.Status,
		)
		logRec.Verdict = result.Verdict
		logRec.Reason = result.Reason
		_ = i.repo.InsertRecoveryLog(ctx, logRec)
		return result, nil
	}

	// Step 4: attempt the CAS transition.
	// Use req.FromGeneration so the caller's snapshot is checked against
	// the DB's current generation. If another actor modified the host since
	// the caller read it, the CAS fails and we log CASFailed.
	updated, err := i.repo.UpdateHostStatus(ctx, req.HostID, req.FromGeneration, newStatus, "")
	if err != nil {
		result.Verdict = db.RecoveryVerdictError
		result.Reason = fmt.Sprintf("UpdateHostStatus error: %v", err)
		logRec.Verdict = result.Verdict
		logRec.Reason = result.Reason
		_ = i.repo.InsertRecoveryLog(ctx, logRec)
		return nil, fmt.Errorf("ExecuteHostRecovery UpdateHostStatus: %w", err)
	}
	if !updated {
		result.Verdict = db.RecoveryVerdictCASFailed
		result.Reason = fmt.Sprintf(
			"CAS failed for host %s: generation mismatch (provided=%d) or status changed since read",
			req.HostID, req.FromGeneration,
		)
		logRec.Verdict = result.Verdict
		logRec.Reason = result.Reason
		_ = i.repo.InsertRecoveryLog(ctx, logRec)
		return result, nil
	}

	// Step 5: log the successful outcome.
	result.Verdict = verdictOnSuccess
	result.Reason = reasonOnSuccess
	logRec.Verdict = result.Verdict
	logRec.Reason = result.Reason
	_ = i.repo.InsertRecoveryLog(ctx, logRec)

	return result, nil
}

// EvaluateCampaignFailedHostRecovery assesses recovery eligibility for all
// hosts in a campaign's failed_host_ids list.
//
// This is an assessment-only operation — it does NOT trigger recovery actions.
// The caller (operator or tooling) uses the assessment to decide which hosts
// to pass to ExecuteHostRecovery, respecting the blast-radius principle by
// choosing how many to act on per call.
//
// For each failed host in the campaign:
//   - If the host is not found → added to NotFound.
//   - If fence_required=TRUE → added to BlockedByFencing.
//   - If status is not in {drained, degraded, unhealthy} → added to NotRecoverable.
//   - Otherwise → added to EligibleHosts.
//
// Returns ErrCampaignNotFound when the campaign does not exist.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator",
//         §interaction_or_ops_contract "Operator initiates a fleet-wide kernel update".
func (i *HostInventory) EvaluateCampaignFailedHostRecovery(ctx context.Context, campaignID string) (*CampaignRecoveryAssessment, error) {
	campaign, err := i.repo.GetCampaignByID(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("EvaluateCampaignFailedHostRecovery: %w", err)
	}

	assessment := &CampaignRecoveryAssessment{
		CampaignID: campaignID,
	}

	// Only inspect hosts in the campaign's failed_host_ids list.
	for _, hostID := range campaign.FailedHostIDs {
		host, err := i.repo.GetHostByID(ctx, hostID)
		if err != nil {
			// Host not found or DB error — treat as not found.
			assessment.NotFound = append(assessment.NotFound, hostID)
			continue
		}

		if host.FenceRequired {
			assessment.BlockedByFencing = append(assessment.BlockedByFencing, host)
			continue
		}

		switch host.Status {
		case "drained", "degraded", "unhealthy":
			assessment.EligibleHosts = append(assessment.EligibleHosts, host)
		default:
			assessment.NotRecoverable = append(assessment.NotRecoverable, host)
		}
	}

	return assessment, nil
}
