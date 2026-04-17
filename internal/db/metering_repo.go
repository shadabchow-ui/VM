package db

// metering_repo.go — Phase 16B: Usage metering and budget persistence.
//
// Implements the exact API required by internal/db/metering_repo_test.go:
//
//   Usage records:
//     InsertUsageRecord(ctx, *UsageRecordRow) → error
//     CloseUsageRecord(ctx, instanceID string, endedAt time.Time) → error
//     GetOpenUsageRecord(ctx, instanceID string) → (*UsageRecordRow, error)
//     ListUsageRecordsByScope(ctx, scopeID string, limit int) → ([]*UsageRecordRow, error)
//
//   Reconciliation holds:
//     AcquireReconciliationHold(ctx, holdID, instanceID, scopeID string, windowStart, windowEnd time.Time) → (bool, error)
//     ApplyReconciliationHold(ctx, holdID string) → error
//     ReleaseReconciliationHold(ctx, instanceID string, windowStart, windowEnd time.Time) → error
//     HoldExistsForWindow(ctx, instanceID string, windowStart, windowEnd time.Time) → (bool, error)
//
//   Budget policies:
//     CreateBudgetPolicy(ctx, id, scopeID, createdBy, enforcementAction string, projectID *string,
//                        limitCents int64, periodStart, periodEnd time.Time, notificationEmail *string)
//                       → (*BudgetPolicyRow, error)
//     GetBudgetPolicyByID(ctx, id string) → (*BudgetPolicyRow, error)
//     ListActiveBudgetPoliciesForScope(ctx, scopeID string) → ([]*BudgetPolicyRow, error)
//     IncrementBudgetAccrual(ctx, scopeID string, deltaCents int64) → error
//     CheckBudgetAllowsCreate(ctx, scopeID string) → error
//
// Domain errors:
//   ErrBudgetExceeded — returned by CheckBudgetAllowsCreate when a block_create policy is exceeded
//
// Constants:
//   UsageRecordTypeStart      = "USAGE_START"
//   UsageRecordTypeEnd        = "USAGE_END"
//   UsageRecordTypeReconciled = "RECONCILED"
//   BudgetActionNotify        = "notify"
//   BudgetActionBlockCreate   = "block_create"
//
// Source: vm-16-02__blueprint__ §mvp, §core_contracts,
//         metering_repo_test.go (authoritative API surface).

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ── Domain errors ─────────────────────────────────────────────────────────────

// ErrBudgetExceeded is returned by CheckBudgetAllowsCreate when an active
// budget policy with enforcement_action='block_create' has accrued_cents >=
// limit_cents for the requesting scope.
//
// Maps to HTTP 422 in handlers — client-correctable (reduce scope usage or
// raise limit). Must NOT be confused with quota_exceeded (count-based) or
// service_unavailable (connectivity).
//
// Source: vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement",
//         instance_errors.go errBudgetExceeded.
var ErrBudgetExceeded = errors.New("budget exceeded: new resource creation blocked by active budget policy")

// ── Usage record constants ────────────────────────────────────────────────────

// Usage record type constants.
// Source: vm-16-02__blueprint__ §components "Metering & Ingestion Subsystem".
const (
	UsageRecordTypeStart      = "USAGE_START"   // VM entered running state
	UsageRecordTypeEnd        = "USAGE_END"      // VM exited running state
	UsageRecordTypeReconciled = "RECONCILED"     // synthesized by reconciliation service
)

// Budget enforcement action constants.
// Source: vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement".
const (
	BudgetActionNotify      = "notify"       // send notification; do not block
	BudgetActionBlockCreate = "block_create" // block new resource creation via quota
)

// ── UsageRecordRow ────────────────────────────────────────────────────────────

// UsageRecordRow is the DB projection of a usage_records row.
//
// Column order is fixed and matches all SELECT queries in this file and the
// usageRecordRow helper in metering_repo_test.go:
//
//	 0: id               string
//	 1: instance_id      string
//	 2: scope_id         string       (owner principal ID at time of event)
//	 3: project_id       *string
//	 4: record_type      string       (UsageRecordType*)
//	 5: instance_type_id string
//	 6: started_at       time.Time
//	 7: ended_at         *time.Time
//	 8: duration_seconds *int64
//	 9: event_id         string       (idempotency key)
//	10: created_at       time.Time
type UsageRecordRow struct {
	ID              string
	InstanceID      string
	ScopeID         string
	ProjectID       *string
	RecordType      string
	InstanceTypeID  string
	StartedAt       time.Time
	EndedAt         *time.Time
	DurationSeconds *int64
	EventID         string
	CreatedAt       time.Time
}

// ── Usage record repo methods ─────────────────────────────────────────────────

// InsertUsageRecord writes a usage record.
// The event_id column has a UNIQUE constraint — duplicate inserts are silently
// discarded (ON CONFLICT DO NOTHING) so callers are idempotent on retry.
//
// Source: vm-16-02__blueprint__ §core_contracts "Event Sourced Single Source of Truth".
func (r *Repo) InsertUsageRecord(ctx context.Context, row *UsageRecordRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO usage_records (
			id, instance_id, scope_id, project_id,
			record_type, instance_type_id,
			started_at, ended_at, duration_seconds,
			event_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (event_id) DO NOTHING
	`,
		row.ID, row.InstanceID, row.ScopeID, row.ProjectID,
		row.RecordType, row.InstanceTypeID,
		row.StartedAt, row.EndedAt, row.DurationSeconds,
		row.EventID,
	)
	if err != nil {
		return fmt.Errorf("InsertUsageRecord: %w", err)
	}
	return nil
}

// CloseUsageRecord sets ended_at and computes duration_seconds for the open
// usage record of an instance.
//
// Idempotent: if the record is already closed (ended_at IS NOT NULL), the
// WHERE clause matches zero rows — no error, no mutation.
//
// Source: vm-16-02__blueprint__ §components "Metering & Ingestion Subsystem".
func (r *Repo) CloseUsageRecord(ctx context.Context, instanceID string, endedAt time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE usage_records
		SET ended_at         = $2,
		    duration_seconds = EXTRACT(EPOCH FROM ($2 - started_at))::BIGINT,
		    updated_at       = NOW()
		WHERE instance_id = $1
		  AND ended_at    IS NULL
	`, instanceID, endedAt)
	if err != nil {
		return fmt.Errorf("CloseUsageRecord: %w", err)
	}
	// Zero rows affected (already closed) is not an error — idempotent.
	return nil
}

// GetOpenUsageRecord returns the current open (ended_at IS NULL) usage record
// for an instance. Returns nil, nil when no open record exists.
func (r *Repo) GetOpenUsageRecord(ctx context.Context, instanceID string) (*UsageRecordRow, error) {
	row := &UsageRecordRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, scope_id, project_id,
		       record_type, instance_type_id,
		       started_at, ended_at, duration_seconds,
		       event_id, created_at
		FROM usage_records
		WHERE instance_id = $1
		  AND ended_at    IS NULL
		LIMIT 1
	`, instanceID).Scan(
		&row.ID, &row.InstanceID, &row.ScopeID, &row.ProjectID,
		&row.RecordType, &row.InstanceTypeID,
		&row.StartedAt, &row.EndedAt, &row.DurationSeconds,
		&row.EventID, &row.CreatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetOpenUsageRecord: %w", err)
	}
	return row, nil
}

// ListUsageRecordsByScope returns usage records for a scope (owner principal),
// newest first. limit is capped at 500 to prevent runaway queries.
func (r *Repo) ListUsageRecordsByScope(ctx context.Context, scopeID string, limit int) ([]*UsageRecordRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, instance_id, scope_id, project_id,
		       record_type, instance_type_id,
		       started_at, ended_at, duration_seconds,
		       event_id, created_at
		FROM usage_records
		WHERE scope_id = $1
		ORDER BY started_at DESC
		LIMIT $2
	`, scopeID, limit)
	if err != nil {
		return nil, fmt.Errorf("ListUsageRecordsByScope: %w", err)
	}
	defer rows.Close()

	var out []*UsageRecordRow
	for rows.Next() {
		row := &UsageRecordRow{}
		if err := rows.Scan(
			&row.ID, &row.InstanceID, &row.ScopeID, &row.ProjectID,
			&row.RecordType, &row.InstanceTypeID,
			&row.StartedAt, &row.EndedAt, &row.DurationSeconds,
			&row.EventID, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListUsageRecordsByScope scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ── Reconciliation holds ──────────────────────────────────────────────────────

// AcquireReconciliationHold atomically writes a hold for an instance/window.
//
// Returns (true, nil) when the hold was inserted.
// Returns (false, nil) when a hold already exists for this window (ON CONFLICT
// DO NOTHING). The caller must NOT synthesize a usage event when false is returned.
//
// Source: vm-16-02__blueprint__ §core_contracts "Reservation-Based Exactly-Once Guarantee",
//         §interaction_or_ops_contract "Reconciliation Service detects a missing window".
func (r *Repo) AcquireReconciliationHold(
	ctx context.Context,
	holdID, instanceID, scopeID string,
	windowStart, windowEnd time.Time,
) (bool, error) {
	res, err := r.pool.Exec(ctx, `
		INSERT INTO reconciliation_holds (
			id, instance_id, scope_id,
			window_start, window_end,
			status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, 'pending', NOW(), NOW())
		ON CONFLICT (instance_id, window_start, window_end) DO NOTHING
	`, holdID, instanceID, scopeID, windowStart, windowEnd)
	if err != nil {
		return false, fmt.Errorf("AcquireReconciliationHold: %w", err)
	}
	return res.RowsAffected() == 1, nil
}

// ApplyReconciliationHold transitions a hold from 'pending' to 'applied',
// indicating that the synthetic usage event has been injected.
//
// Returns an error when zero rows are updated (hold not found or not in
// 'pending' state).
//
// Source: vm-16-02__blueprint__ §core_contracts "Reservation-Based Exactly-Once Guarantee".
func (r *Repo) ApplyReconciliationHold(ctx context.Context, holdID string) error {
	res, err := r.pool.Exec(ctx, `
		UPDATE reconciliation_holds
		SET status     = 'applied',
		    updated_at = NOW()
		WHERE id     = $1
		  AND status = 'pending'
	`, holdID)
	if err != nil {
		return fmt.Errorf("ApplyReconciliationHold: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("ApplyReconciliationHold %s: hold not found or not in pending state", holdID)
	}
	return nil
}

// ReleaseReconciliationHold removes a hold for an instance/window when the
// original usage event arrived late (voiding the hold).
// Idempotent — deleting an already-deleted or non-existent hold is not an error.
//
// Source: vm-16-02__blueprint__ §core_contracts "Reservation-Based Exactly-Once Guarantee".
func (r *Repo) ReleaseReconciliationHold(
	ctx context.Context,
	instanceID string,
	windowStart, windowEnd time.Time,
) error {
	_, err := r.pool.Exec(ctx, `
		DELETE FROM reconciliation_holds
		WHERE instance_id  = $1
		  AND window_start = $2
		  AND window_end   = $3
	`, instanceID, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("ReleaseReconciliationHold: %w", err)
	}
	return nil
}

// HoldExistsForWindow reports whether a reconciliation hold exists for the
// given instance and time window.
//
// Source: vm-16-02__blueprint__ §core_contracts "Reservation-Based Exactly-Once Guarantee".
func (r *Repo) HoldExistsForWindow(
	ctx context.Context,
	instanceID string,
	windowStart, windowEnd time.Time,
) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM reconciliation_holds
			WHERE instance_id  = $1
			  AND window_start = $2
			  AND window_end   = $3
		)
	`, instanceID, windowStart, windowEnd).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("HoldExistsForWindow: %w", err)
	}
	return exists, nil
}

// ── BudgetPolicyRow ───────────────────────────────────────────────────────────

// BudgetPolicyRow is the DB projection of a budget_policies row.
//
// Column order is fixed and matches all SELECT queries in this file and the
// budgetPolicyRow helper in metering_repo_test.go:
//
//	 0: id                  string
//	 1: scope_id            string
//	 2: project_id          *string
//	 3: limit_cents         int64
//	 4: accrued_cents       int64
//	 5: period_start        time.Time
//	 6: period_end          time.Time
//	 7: enforcement_action  string     (BudgetAction*)
//	 8: notification_email  *string
//	 9: status              string
//	10: created_by          string
//	11: created_at          time.Time
//	12: updated_at          time.Time
type BudgetPolicyRow struct {
	ID                string
	ScopeID           string
	ProjectID         *string
	LimitCents        int64
	AccruedCents      int64
	PeriodStart       time.Time
	PeriodEnd         time.Time
	EnforcementAction string
	NotificationEmail *string
	Status            string
	CreatedBy         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ── Budget policy repo methods ────────────────────────────────────────────────

// CreateBudgetPolicy inserts a new budget policy and returns the freshly fetched row.
//
// Source: vm-16-02__blueprint__ §components "Budget & Quota Enforcement Subsystem".
func (r *Repo) CreateBudgetPolicy(
	ctx context.Context,
	id, scopeID, createdBy, enforcementAction string,
	projectID *string,
	limitCents int64,
	periodStart, periodEnd time.Time,
	notificationEmail *string,
) (*BudgetPolicyRow, error) {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO budget_policies (
			id, scope_id, project_id, limit_cents, accrued_cents,
			period_start, period_end,
			enforcement_action, notification_email,
			status, created_by, created_at, updated_at
		) VALUES ($1, $2, $3, $4, 0, $5, $6, $7, $8, 'active', $9, NOW(), NOW())
	`,
		id, scopeID, projectID, limitCents,
		periodStart, periodEnd,
		enforcementAction, notificationEmail,
		createdBy,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateBudgetPolicy: %w", err)
	}
	return r.GetBudgetPolicyByID(ctx, id)
}

// GetBudgetPolicyByID fetches a budget policy by primary key.
// Returns nil, nil when not found.
func (r *Repo) GetBudgetPolicyByID(ctx context.Context, id string) (*BudgetPolicyRow, error) {
	bp := &BudgetPolicyRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, scope_id, project_id,
		       limit_cents, accrued_cents,
		       period_start, period_end,
		       enforcement_action, notification_email,
		       status, created_by, created_at, updated_at
		FROM budget_policies
		WHERE id = $1
	`, id).Scan(
		&bp.ID, &bp.ScopeID, &bp.ProjectID,
		&bp.LimitCents, &bp.AccruedCents,
		&bp.PeriodStart, &bp.PeriodEnd,
		&bp.EnforcementAction, &bp.NotificationEmail,
		&bp.Status, &bp.CreatedBy, &bp.CreatedAt, &bp.UpdatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetBudgetPolicyByID: %w", err)
	}
	return bp, nil
}

// ListActiveBudgetPoliciesForScope returns all active budget policies for a scope.
// Returns an empty (non-nil) slice when none exist.
func (r *Repo) ListActiveBudgetPoliciesForScope(ctx context.Context, scopeID string) ([]*BudgetPolicyRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, scope_id, project_id,
		       limit_cents, accrued_cents,
		       period_start, period_end,
		       enforcement_action, notification_email,
		       status, created_by, created_at, updated_at
		FROM budget_policies
		WHERE scope_id = $1
		  AND status   = 'active'
		  AND period_end > NOW()
		ORDER BY created_at ASC
	`, scopeID)
	if err != nil {
		return nil, fmt.Errorf("ListActiveBudgetPoliciesForScope: %w", err)
	}
	defer rows.Close()

	out := []*BudgetPolicyRow{} // non-nil even when empty
	for rows.Next() {
		bp := &BudgetPolicyRow{}
		if err := rows.Scan(
			&bp.ID, &bp.ScopeID, &bp.ProjectID,
			&bp.LimitCents, &bp.AccruedCents,
			&bp.PeriodStart, &bp.PeriodEnd,
			&bp.EnforcementAction, &bp.NotificationEmail,
			&bp.Status, &bp.CreatedBy, &bp.CreatedAt, &bp.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListActiveBudgetPoliciesForScope scan: %w", err)
		}
		out = append(out, bp)
	}
	return out, rows.Err()
}

// IncrementBudgetAccrual adds deltaCents to accrued_cents on all active budget
// policies for a scope that are within their period. Called by the metering
// pipeline after rating each usage event.
//
// Source: vm-16-02__blueprint__ §components "Budget & Quota Enforcement Subsystem".
func (r *Repo) IncrementBudgetAccrual(ctx context.Context, scopeID string, deltaCents int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE budget_policies
		SET accrued_cents = accrued_cents + $2,
		    updated_at    = NOW()
		WHERE scope_id   = $1
		  AND status     = 'active'
		  AND period_end > NOW()
	`, scopeID, deltaCents)
	if err != nil {
		return fmt.Errorf("IncrementBudgetAccrual: %w", err)
	}
	return nil
}

// CheckBudgetAllowsCreate returns nil when no active block_create policy is
// exceeded for the scope. Returns ErrBudgetExceeded when at least one policy
// has accrued_cents >= limit_cents and enforcement_action = 'block_create'.
//
// This is called from the instance create handler before admitting a new VM.
// Distinguishes budget enforcement from quota admission (errQuotaExceeded).
//
// Source: vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement",
//         instance_errors.go errBudgetExceeded.
func (r *Repo) CheckBudgetAllowsCreate(ctx context.Context, scopeID string) error {
	var exceeded bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM budget_policies
			WHERE scope_id           = $1
			  AND status             = 'active'
			  AND enforcement_action = 'block_create'
			  AND accrued_cents      >= limit_cents
			  AND period_end         > NOW()
		)
	`, scopeID).Scan(&exceeded)
	if err != nil {
		return fmt.Errorf("CheckBudgetAllowsCreate: %w", err)
	}
	if exceeded {
		return ErrBudgetExceeded
	}
	return nil
}
