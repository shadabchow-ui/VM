package db

// host_recovery.go — DB methods for VM-P2E Slice 6 recovery automation.
//
// VM-P2E Slice 6 additions:
//   - RecoveryLogRecord: struct representing a host_recovery_log row.
//   - RecoveryVerdict: typed constants for verdict values.
//   - InsertRecoveryLog: append-only insert into host_recovery_log.
//   - GetRecoveryEligibleHosts: scans hosts that may be safe to recover
//     (fence_required=FALSE, status in recoverable set).
//   - GetHostRecoveryLog: per-host history for operator inspection.
//
// Design decisions:
//   - host_recovery_log rows are append-only; no UPDATE path exists.
//     This preserves full audit history of what automation decided.
//   - GetRecoveryEligibleHosts deliberately excludes fence_required=TRUE
//     hosts. Recovery automation MUST NOT act on those hosts — the DB
//     query is the canonical gate, not a runtime if-statement.
//   - Recoverable statuses are: "drained", "degraded", "unhealthy".
//     "retired" and "retiring" are terminal/in-progress and not recoverable here.
//     "ready" hosts need no recovery. "draining" hosts are mid-drain; recovery
//     actions on them would race the drain and are excluded.
//   - InsertRecoveryLog takes a *RecoveryLogRecord so callers can fill all
//     fields explicitly; no hidden defaults except attempted_at=NOW().
//
// Source: vm-13-03__skill__ §instructions "fence-then-recover" protocol,
//         vm-13-03__blueprint__ §"Fencing Decision Logic",
//         §interaction_or_ops_contract "HA VM recovery after host failure".

import (
	"context"
	"fmt"
	"time"
)

// ── Recovery verdict constants ─────────────────────────────────────────────────
//
// These are the canonical verdict values stored in host_recovery_log.verdict.
// Each value describes what the recovery automation decided and why.
const (
	// RecoveryVerdictSkippedFenceRequired: host has fence_required=TRUE.
	// Recovery automation explicitly blocked — STONITH must complete first.
	// This is the most important safety gate in the whole slice.
	RecoveryVerdictSkippedFenceRequired = "skipped_fence_required"

	// RecoveryVerdictSkippedNotEligible: host status is not recoverable
	// (e.g. retiring, retired, ready, draining). No action taken.
	RecoveryVerdictSkippedNotEligible = "skipped_not_eligible"

	// RecoveryVerdictReactivated: drained host transitioned back to ready.
	// The host can now accept new VM placements again.
	RecoveryVerdictReactivated = "reactivated"

	// RecoveryVerdictDrainInitiated: degraded or unhealthy host transitioned
	// to draining for the drain-then-recover path. The host will drain its
	// active workload before becoming eligible for reactivation.
	RecoveryVerdictDrainInitiated = "drain_initiated"

	// RecoveryVerdictCASFailed: CAS generation mismatch — the host was
	// concurrently modified between the read and the attempted write.
	// No state was changed. The caller may retry after re-reading generation.
	RecoveryVerdictCASFailed = "cas_failed"

	// RecoveryVerdictError: an unexpected DB error prevented the recovery action.
	// The verdict is logged; the operator should investigate.
	RecoveryVerdictError = "error"
)

// recoverableStatuses is the set of host statuses from which recovery actions
// are permitted (subject to the fence_required=FALSE gate).
//
// "drained"   → candidate for reactivation (→ ready).
// "degraded"  → candidate for drain-then-recover (→ draining).
// "unhealthy" → candidate for drain-then-recover (→ draining), only after
//
//	fence_required is cleared. GetRecoveryEligibleHosts already enforces
//	fence_required=FALSE so this set can include "unhealthy" safely.
var recoverableStatuses = []string{"drained", "degraded", "unhealthy"}

// ── RecoveryLogRecord ──────────────────────────────────────────────────────────

// RecoveryLogRecord is the struct representation of a host_recovery_log row.
// Rows are append-only audit records — never updated after insert.
type RecoveryLogRecord struct {
	ID                       string
	HostID                   string
	Verdict                  string
	Reason                   string
	HostStatusAtAttempt      string
	HostGenerationAtAttempt  int64
	FenceRequiredAtAttempt   bool
	Actor                    string    // "operator" | "recovery_loop"
	CampaignID               *string   // nil when not campaign-scoped
	AttemptedAt              time.Time
}

// ── DB methods ────────────────────────────────────────────────────────────────

// InsertRecoveryLog appends a recovery decision record to host_recovery_log.
//
// Rows are immutable after insert — this method has no UPDATE path.
// The id field must be caller-generated (e.g. idgen.New(idgen.PrefixEvent)).
// attempted_at is set to NOW() by the DB; the field on the record is ignored
// on insert but returned if the caller re-reads.
//
// Returns an error only on DB write failure; all verdict values are valid.
//
// Source: vm-13-03__blueprint__ §"Fencing Controller" (audit trail requirement).
func (r *Repo) InsertRecoveryLog(ctx context.Context, rec *RecoveryLogRecord) error {
	var campaignVal interface{}
	if rec.CampaignID != nil && *rec.CampaignID != "" {
		campaignVal = *rec.CampaignID
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO host_recovery_log (
			id, host_id, verdict, reason,
			host_status_at_attempt, host_generation_at_attempt,
			fence_required_at_attempt, actor, campaign_id,
			attempted_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
	`,
		rec.ID,
		rec.HostID,
		rec.Verdict,
		rec.Reason,
		rec.HostStatusAtAttempt,
		rec.HostGenerationAtAttempt,
		rec.FenceRequiredAtAttempt,
		rec.Actor,
		campaignVal,
	)
	if err != nil {
		return fmt.Errorf("InsertRecoveryLog host_id=%s: %w", rec.HostID, err)
	}
	return nil
}

// GetRecoveryEligibleHosts returns hosts that are safe candidates for recovery automation.
//
// Eligibility criteria (all must be true):
//   - status IN ('drained', 'degraded', 'unhealthy')
//   - fence_required = FALSE  ← the hard fencing gate
//
// The fence_required=FALSE predicate in the SQL is the canonical enforcement
// point. Recovery automation that calls this method will never see
// fence_required=TRUE hosts in the result — they are excluded at the DB layer,
// not filtered at the application layer.
//
// Results are ordered by updated_at ASC (oldest-touched first), consistent
// with the priority convention used for degraded/unhealthy monitoring.
//
// This is a pure read — no state mutations.
//
// Source: vm-13-03__skill__ §instructions "fence-then-recover" protocol,
//         vm-13-03__blueprint__ §"Fencing Decision Logic".
func (r *Repo) GetRecoveryEligibleHosts(ctx context.Context) ([]*HostRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, availability_zone, status,
		       generation, drain_reason, reason_code, fence_required, retired_at,
		       total_cpu, total_memory_mb, total_disk_gb,
		       used_cpu, used_memory_mb, used_disk_gb,
		       agent_version, last_heartbeat_at, registered_at, updated_at
		FROM hosts
		WHERE status = ANY($1)
		  AND fence_required = FALSE
		ORDER BY updated_at ASC
	`, recoverableStatuses)
	if err != nil {
		return nil, fmt.Errorf("GetRecoveryEligibleHosts: %w", err)
	}
	defer rows.Close()

	var hosts []*HostRecord
	for rows.Next() {
		h := &HostRecord{}
		if err := rows.Scan(
			&h.ID, &h.AvailabilityZone, &h.Status,
			&h.Generation, &h.DrainReason, &h.ReasonCode, &h.FenceRequired, &h.RetiredAt,
			&h.TotalCPU, &h.TotalMemoryMB, &h.TotalDiskGB,
			&h.UsedCPU, &h.UsedMemoryMB, &h.UsedDiskGB,
			&h.AgentVersion, &h.LastHeartbeatAt, &h.RegisteredAt, &h.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("GetRecoveryEligibleHosts scan: %w", err)
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

// GetHostRecoveryLog returns the recovery attempt history for a single host,
// ordered by attempted_at DESC (most recent first).
//
// This is the operator inspection surface: an operator who wants to understand
// why a host was or was not recovered can call this endpoint without scanning
// the full log table.
//
// Returns an empty slice (not an error) when the host has no log entries.
//
// Source: vm-13-03__blueprint__ §"Fencing Controller" (operator inspection).
func (r *Repo) GetHostRecoveryLog(ctx context.Context, hostID string) ([]*RecoveryLogRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, host_id, verdict, reason,
		       host_status_at_attempt, host_generation_at_attempt,
		       fence_required_at_attempt, actor, campaign_id,
		       attempted_at
		FROM host_recovery_log
		WHERE host_id = $1
		ORDER BY attempted_at DESC
	`, hostID)
	if err != nil {
		return nil, fmt.Errorf("GetHostRecoveryLog: %w", err)
	}
	defer rows.Close()

	var records []*RecoveryLogRecord
	for rows.Next() {
		rec := &RecoveryLogRecord{}
		if err := rows.Scan(
			&rec.ID, &rec.HostID, &rec.Verdict, &rec.Reason,
			&rec.HostStatusAtAttempt, &rec.HostGenerationAtAttempt,
			&rec.FenceRequiredAtAttempt, &rec.Actor, &rec.CampaignID,
			&rec.AttemptedAt,
		); err != nil {
			return nil, fmt.Errorf("GetHostRecoveryLog scan: %w", err)
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}
