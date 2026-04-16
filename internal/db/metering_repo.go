package db

// metering_repo.go — Usage metering and budget controls persistence.
//
// Phase 16A: Smallest correct repo-native seams for:
//   - vm-16-02: Usage metering, billing, and budget controls.
//
// What this file adds:
//   - UsageRecordRow       — DB projection of usage_records
//   - ReconciliationHold   — DB projection of reconciliation_holds
//   - BudgetPolicyRow      — DB projection of budget_policies
//   - Repo methods for usage attribution, reconciliation, and budget enforcement
//
// Architecture decisions locked here:
//
//  1. Usage records are the single source of truth for billing (event-sourced).
//     Invoices are projections of this log; corrections are new compensating records.
//     Source: vm-16-02__blueprint__ §core_contracts "Event Sourced Single Source of Truth".
//
//  2. Reconciliation uses a reservation-based exactly-once protocol.
//     A RECONCILIATION_HOLD is written before any synthetic usage event is injected,
//     preventing double-billing from late-arriving original events.
//     Source: vm-16-02__blueprint__ §core_contracts "Reservation-Based Exactly-Once Guarantee".
//
//  3. Budget enforcement is non-destructive: it blocks NEW resource creation by
//     setting a quota lock (CheckBudgetAllowsCreate returns ErrBudgetExceeded →
//     HTTP 422 budget_exceeded), but NEVER terminates running instances.
//     Source: vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement".
//
//  4. Usage attribution scope: owner_principal_id (which equals project.principal_id
//     when a project scope is used, or the user's principal_id in classic mode).
//     This matches the existing quota scope model — no new scope anchor is introduced.
//     Source: instance_handlers.go §"VM-P2D Slice 4: Project scope resolution".
//
// What is intentionally deferred to Phase 16B / future phases:
//   - Dual-path (Lambda) processing pipeline (streaming + batch)
//   - Rating & Pricing Subsystem (versioned pricing catalog + cache)
//   - Automated DLQ triage
//   - Full invoice generation
//   - Real-time cost visibility (Druid/ClickHouse OLAP path)
//   - Automated budget quota lock + rollback (Budget & Quota Enforcement Subsystem)
//
// Schema requirements (migration 018_phase16a_metering.up.sql).
// See the migration file for the full DDL.
//
// Source: vm-16-02__blueprint__ §components, §core_contracts,
//
//	vm-16-02__research__ §"Metering & Ingestion Subsystem",
//	vm-16-02__research__ §"Budget & Quota Enforcement Subsystem".

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ── UsageRecordRow ────────────────────────────────────────────────────────────

// UsageRecordRow is the DB projection of a usage_records row.
//
// usage_records is the immutable log of all billable consumption events.
// Rows are INSERT-only — never UPDATE or DELETE. Corrections are new rows with
// record_type='ADJUSTMENT'.
//
// Column order is fixed and must match all SELECT queries in this file.
type UsageRecordRow struct {
	ID              string
	InstanceID      string
	ScopeID         string     // owner_principal_id at the time of the event
	ProjectID       *string    // nil for classic/no-project mode
	RecordType      string     // USAGE_START | USAGE_END | RECONCILED | ADJUSTMENT
	InstanceTypeID  string
	StartedAt       time.Time
	EndedAt         *time.Time // nil for USAGE_START records
	DurationSeconds *int64     // nil until EndedAt is set
	EventID         string     // idempotency key — unique per metering event
	CreatedAt       time.Time
}

// Usage record type constants.
// Source: vm-16-02__blueprint__ §components "Metering & Ingestion Subsystem".
const (
	UsageRecordTypeStart      = "USAGE_START"
	UsageRecordTypeEnd        = "USAGE_END"
	UsageRecordTypeReconciled = "RECONCILED" // synthesized by reconciliation service
	UsageRecordTypeAdjustment = "ADJUSTMENT" // compensating event for corrections
)

// ── ReconciliationHoldRow ─────────────────────────────────────────────────────

// ReconciliationHoldRow is the DB projection of a reconciliation_holds row.
//
// A hold is written by the reconciliation service BEFORE it synthesizes a
// missing usage event. This prevents double-billing: if the original event
// arrives late, the ingestion path discards it because a hold exists.
//
// Source: vm-16-02__blueprint__ §core_contracts "Reservation-Based Exactly-Once Guarantee",
//
//	§interaction_or_ops_contract "The Reconciliation Service detects a missing window".
type ReconciliationHoldRow struct {
	ID         string
	InstanceID string
	ScopeID    string
	WindowStart time.Time
	WindowEnd   time.Time
	Status     string // pending | applied | released
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Reconciliation hold status constants.
const (
	ReconciliationHoldPending  = "pending"  // hold placed, synthetic event not yet written
	ReconciliationHoldApplied  = "applied"  // synthetic event written successfully
	ReconciliationHoldReleased = "released" // original event arrived; hold released
)

// ── BudgetPolicyRow ───────────────────────────────────────────────────────────

// BudgetPolicyRow is the DB projection of a budget_policies row.
//
// A budget policy defines a spending cap for a scope (project or user principal).
// When accrued_amount_cents reaches limit_cents, CheckBudgetAllowsCreate returns
// ErrBudgetExceeded. The system NEVER terminates running instances — only new
// resource creation is blocked.
//
// Source: vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement",
//
//	§components "Budget & Quota Enforcement Subsystem".
type BudgetPolicyRow struct {
	ID                 string
	ScopeID            string  // owner_principal_id the policy applies to
	ProjectID          *string // nil for user-level policies
	LimitCents         int64   // spending cap in cents (USD); 0 = disabled
	AccruedCents       int64   // current period spend (best-effort streaming path)
	PeriodStart        time.Time
	PeriodEnd          time.Time
	EnforcementAction  string // notify | block_create (Phase 16A: notify only implemented)
	NotificationEmail  *string
	Status             string // active | paused | expired
	CreatedBy          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Budget policy enforcement action constants.
// Source: vm-16-02__blueprint__ §components "Budget & Quota Enforcement Subsystem".
const (
	BudgetActionNotify      = "notify"       // send notification, do not block
	BudgetActionBlockCreate = "block_create" // block new resource creation via quota lock
)

// ErrBudgetExceeded is returned by CheckBudgetAllowsCreate when the scope has
// an active budget policy in force with enforcement_action='block_create' and
// accrued spend has reached the limit.
//
// This must NOT be conflated with ErrQuotaExceeded (count-based) or scheduler
// capacity errors. Each maps to a distinct HTTP error code.
//
// Source: vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement".
var ErrBudgetExceeded = fmt.Errorf("budget limit exceeded for this scope")

// ── Usage Record repo methods ─────────────────────────────────────────────────

// InsertUsageRecord appends an immutable usage record to the usage_records log.
//
// eventID is the idempotency key — ON CONFLICT DO NOTHING ensures at-most-once
// insertion for duplicate metering events (agent retries, reconciler races).
//
// scopeID must equal the instance's owner_principal_id at the time of recording.
// projectID is nil for classic/no-project instances.
//
// Source: vm-16-02__blueprint__ §core_contracts "Event Sourced Single Source of Truth",
//
//	§interaction_or_ops_contract "ingestion receives duplicate eventId → discard".
func (r *Repo) InsertUsageRecord(ctx context.Context, row *UsageRecordRow) error {
	const q = `
INSERT INTO usage_records (
  id, instance_id, scope_id, project_id, record_type,
  instance_type_id, started_at, ended_at, duration_seconds, event_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (event_id) DO NOTHING`
	if _, err := r.pool.Exec(ctx, q,
		row.ID, row.InstanceID, row.ScopeID, row.ProjectID, row.RecordType,
		row.InstanceTypeID, row.StartedAt, row.EndedAt, row.DurationSeconds, row.EventID,
	); err != nil {
		return fmt.Errorf("InsertUsageRecord: %w", err)
	}
	return nil
}

// CloseUsageRecord sets ended_at and duration_seconds on a USAGE_START record,
// transitioning it to a closed billing interval.
//
// This is called when an instance stops, is deleted, or fails. It is idempotent:
// calling again after ended_at is already set is a no-op (WHERE ended_at IS NULL).
//
// Source: vm-16-02__blueprint__ §components "Metering & Ingestion Subsystem".
func (r *Repo) CloseUsageRecord(ctx context.Context, instanceID string, endedAt time.Time) error {
	const q = `
UPDATE usage_records
SET ended_at         = $2,
    duration_seconds = EXTRACT(EPOCH FROM ($2 - started_at))::BIGINT,
    updated_at       = NOW()
WHERE instance_id = $1
  AND record_type = 'USAGE_START'
  AND ended_at    IS NULL`
	if _, err := r.pool.Exec(ctx, q, instanceID, endedAt); err != nil {
		return fmt.Errorf("CloseUsageRecord: %w", err)
	}
	return nil
}

// GetOpenUsageRecord returns the open (ended_at IS NULL) USAGE_START record
// for an instance. Returns nil, nil when no open record exists.
//
// Used by the reconciliation service to determine whether a gap window needs
// a RECONCILIATION_HOLD.
//
// Source: vm-16-02__blueprint__ §components "Reconciliation Service".
func (r *Repo) GetOpenUsageRecord(ctx context.Context, instanceID string) (*UsageRecordRow, error) {
	const q = `
SELECT id, instance_id, scope_id, project_id, record_type,
       instance_type_id, started_at, ended_at, duration_seconds, event_id, created_at
FROM usage_records
WHERE instance_id = $1
  AND record_type = 'USAGE_START'
  AND ended_at    IS NULL
LIMIT 1`
	rec := &UsageRecordRow{}
	err := r.pool.QueryRow(ctx, q, instanceID).Scan(
		&rec.ID, &rec.InstanceID, &rec.ScopeID, &rec.ProjectID, &rec.RecordType,
		&rec.InstanceTypeID, &rec.StartedAt, &rec.EndedAt, &rec.DurationSeconds,
		&rec.EventID, &rec.CreatedAt,
	)
	if err != nil {
		if isMeteringNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetOpenUsageRecord: %w", err)
	}
	return rec, nil
}

// ListUsageRecordsByScope returns usage records for a scope (owner_principal_id),
// ordered by started_at DESC. Limit is capped at 500.
//
// Used for billing summary and reconciliation audit queries.
//
// Source: vm-16-02__blueprint__ §core_contracts "Event Sourced Single Source of Truth".
func (r *Repo) ListUsageRecordsByScope(ctx context.Context, scopeID string, limit int) ([]*UsageRecordRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	const q = `
SELECT id, instance_id, scope_id, project_id, record_type,
       instance_type_id, started_at, ended_at, duration_seconds, event_id, created_at
FROM usage_records
WHERE scope_id = $1
ORDER BY started_at DESC
LIMIT $2`
	rows, err := r.pool.Query(ctx, q, scopeID, limit)
	if err != nil {
		return nil, fmt.Errorf("ListUsageRecordsByScope: %w", err)
	}
	defer rows.Close()

	var out []*UsageRecordRow
	for rows.Next() {
		rec := &UsageRecordRow{}
		if err := rows.Scan(
			&rec.ID, &rec.InstanceID, &rec.ScopeID, &rec.ProjectID, &rec.RecordType,
			&rec.InstanceTypeID, &rec.StartedAt, &rec.EndedAt, &rec.DurationSeconds,
			&rec.EventID, &rec.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListUsageRecordsByScope scan: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListUsageRecordsByScope rows err: %w", err)
	}
	return out, nil
}

// ── Reconciliation Hold repo methods ──────────────────────────────────────────

// AcquireReconciliationHold atomically writes a RECONCILIATION_HOLD for the
// given (instanceID, windowStart, windowEnd) if one does not already exist.
//
// Returns (true, nil) when the hold was successfully created (caller may now
// synthesize a usage event).
// Returns (false, nil) when a hold already exists for this window (another
// reconciler instance raced ahead — caller must not synthesize a duplicate).
//
// This is the "reserve before synthesize" phase of the exactly-once protocol.
//
// Source: vm-16-02__blueprint__ §core_contracts "Reservation-Based Exactly-Once Guarantee",
//
//	§interaction_or_ops_contract "Reconciliation Service detects missing window".
func (r *Repo) AcquireReconciliationHold(
	ctx context.Context,
	id, instanceID, scopeID string,
	windowStart, windowEnd time.Time,
) (bool, error) {
	const q = `
INSERT INTO reconciliation_holds (id, instance_id, scope_id, window_start, window_end, status)
VALUES ($1, $2, $3, $4, $5, 'pending')
ON CONFLICT (instance_id, window_start, window_end) DO NOTHING`
	tag, err := r.pool.Exec(ctx, q, id, instanceID, scopeID, windowStart, windowEnd)
	if err != nil {
		return false, fmt.Errorf("AcquireReconciliationHold: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ApplyReconciliationHold marks a hold as applied (synthetic event was written).
//
// Source: vm-16-02__blueprint__ §components "Reconciliation Service".
func (r *Repo) ApplyReconciliationHold(ctx context.Context, id string) error {
	const q = `
UPDATE reconciliation_holds
SET status = 'applied', updated_at = NOW()
WHERE id = $1 AND status = 'pending'`
	tag, err := r.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("ApplyReconciliationHold: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ApplyReconciliationHold %s: hold not found or not pending", id)
	}
	return nil
}

// ReleaseReconciliationHold marks a hold as released (original event arrived
// late — no synthetic event should be processed).
//
// Source: vm-16-02__blueprint__ §interaction_or_ops_contract
//
//	"ingestion receives eventId for a reserved slot → discard".
func (r *Repo) ReleaseReconciliationHold(ctx context.Context, instanceID string, windowStart, windowEnd time.Time) error {
	const q = `
UPDATE reconciliation_holds
SET status = 'released', updated_at = NOW()
WHERE instance_id   = $1
  AND window_start  = $2
  AND window_end    = $3
  AND status        = 'pending'`
	if _, err := r.pool.Exec(ctx, q, instanceID, windowStart, windowEnd); err != nil {
		return fmt.Errorf("ReleaseReconciliationHold: %w", err)
	}
	return nil
}

// HoldExistsForWindow reports whether an active (pending or applied) hold exists
// for the given instance window. The ingestion path calls this before accepting
// a late-arriving usage event to prevent double-billing.
//
// Source: vm-16-02__blueprint__ §interaction_or_ops_contract
//
//	"ingestion receives eventId after RECONCILIATION_HOLD → discard".
func (r *Repo) HoldExistsForWindow(ctx context.Context, instanceID string, windowStart, windowEnd time.Time) (bool, error) {
	const q = `
SELECT EXISTS(
  SELECT 1 FROM reconciliation_holds
  WHERE instance_id  = $1
    AND window_start = $2
    AND window_end   = $3
    AND status IN ('pending', 'applied')
)`
	var exists bool
	if err := r.pool.QueryRow(ctx, q, instanceID, windowStart, windowEnd).Scan(&exists); err != nil {
		return false, fmt.Errorf("HoldExistsForWindow: %w", err)
	}
	return exists, nil
}

// ── Budget Policy repo methods ────────────────────────────────────────────────

// CreateBudgetPolicy inserts a new budget_policies row.
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
	const q = `
INSERT INTO budget_policies (
  id, scope_id, project_id, limit_cents, accrued_cents,
  period_start, period_end, enforcement_action, notification_email, status, created_by
) VALUES ($1, $2, $3, $4, 0, $5, $6, $7, $8, 'active', $9)`
	if _, err := r.pool.Exec(ctx, q,
		id, scopeID, projectID, limitCents,
		periodStart, periodEnd, enforcementAction, notificationEmail, createdBy,
	); err != nil {
		return nil, fmt.Errorf("CreateBudgetPolicy: %w", err)
	}
	return r.GetBudgetPolicyByID(ctx, id)
}

// GetBudgetPolicyByID fetches a budget policy by its primary key.
// Returns nil, nil when not found.
//
// Source: vm-16-02__blueprint__ §components "Budget & Quota Enforcement Subsystem".
func (r *Repo) GetBudgetPolicyByID(ctx context.Context, id string) (*BudgetPolicyRow, error) {
	const q = `
SELECT id, scope_id, project_id, limit_cents, accrued_cents,
       period_start, period_end, enforcement_action, notification_email, status, created_by, created_at, updated_at
FROM budget_policies
WHERE id = $1`
	bp, err := scanBudgetPolicy(r.pool.QueryRow(ctx, q, id))
	if err != nil {
		if isMeteringNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetBudgetPolicyByID: %w", err)
	}
	return bp, nil
}

// ListActiveBudgetPoliciesForScope returns all active budget policies whose
// scope_id equals the given scopeID and whose period_end is in the future.
//
// Returns an empty slice (not nil) when no policies exist.
//
// Source: vm-16-02__blueprint__ §components "Budget & Quota Enforcement Subsystem".
func (r *Repo) ListActiveBudgetPoliciesForScope(ctx context.Context, scopeID string) ([]*BudgetPolicyRow, error) {
	const q = `
SELECT id, scope_id, project_id, limit_cents, accrued_cents,
       period_start, period_end, enforcement_action, notification_email, status, created_by, created_at, updated_at
FROM budget_policies
WHERE scope_id  = $1
  AND status    = 'active'
  AND period_end > NOW()
ORDER BY created_at ASC`
	rows, err := r.pool.Query(ctx, q, scopeID)
	if err != nil {
		return nil, fmt.Errorf("ListActiveBudgetPoliciesForScope: %w", err)
	}
	defer rows.Close()

	out := make([]*BudgetPolicyRow, 0)
	for rows.Next() {
		bp, err := scanBudgetPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("ListActiveBudgetPoliciesForScope scan: %w", err)
		}
		out = append(out, bp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListActiveBudgetPoliciesForScope rows err: %w", err)
	}
	return out, nil
}

// IncrementBudgetAccrual adds deltaCents to accrued_cents for all active budget
// policies in the given scope for the current period.
//
// Called by the metering path when a usage record is closed (usage interval ends).
// This is a best-effort increment — the accrued_cents value on the streaming
// path is approximate; the authoritative figure is computed from the usage_records
// log in the batch path.
//
// Source: vm-16-02__blueprint__ §components "Dual-Path Processing Pipeline" (streaming).
func (r *Repo) IncrementBudgetAccrual(ctx context.Context, scopeID string, deltaCents int64) error {
	const q = `
UPDATE budget_policies
SET accrued_cents = accrued_cents + $2,
    updated_at    = NOW()
WHERE scope_id  = $1
  AND status    = 'active'
  AND period_end > NOW()`
	if _, err := r.pool.Exec(ctx, q, scopeID, deltaCents); err != nil {
		return fmt.Errorf("IncrementBudgetAccrual: %w", err)
	}
	return nil
}

// CheckBudgetAllowsCreate returns ErrBudgetExceeded if the scope has an active
// budget policy with enforcement_action='block_create' and accrued_cents has
// reached or exceeded limit_cents.
//
// Returns nil when:
//   - No active budget policies exist for the scope.
//   - All active policies have enforcement_action='notify' (never blocks).
//   - All block_create policies still have headroom (accrued < limit).
//
// This is called at the admission gate in handleCreateInstance, after the quota
// check and before InsertInstance, to ensure budget enforcement is non-destructive
// (only new resource creation is blocked).
//
// Error separation:
//   - ErrQuotaExceeded → count-based limit (vm-13-02)
//   - ErrBudgetExceeded → dollar-based limit (vm-16-02)
//   These are distinct and must never be collapsed. Each maps to a different
//   error code and user-visible message.
//
// Source: vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement",
//
//	§interaction_or_ops_contract "budget crosses threshold with APPLY_QUOTA_LOCK action".
func (r *Repo) CheckBudgetAllowsCreate(ctx context.Context, scopeID string) error {
	const q = `
SELECT EXISTS(
  SELECT 1 FROM budget_policies
  WHERE scope_id          = $1
    AND status            = 'active'
    AND period_end        > NOW()
    AND enforcement_action = 'block_create'
    AND limit_cents       > 0
    AND accrued_cents     >= limit_cents
)`
	var exceeded bool
	if err := r.pool.QueryRow(ctx, q, scopeID).Scan(&exceeded); err != nil {
		return fmt.Errorf("CheckBudgetAllowsCreate: %w", err)
	}
	if exceeded {
		return ErrBudgetExceeded
	}
	return nil
}

// ── scan helpers ──────────────────────────────────────────────────────────────

// scanBudgetPolicy scans a single Row or Rows entry into a BudgetPolicyRow.
// Accepts both db.Row (from QueryRow) and db.Rows (from Query via rows.Next()).
type budgetPolicyScanner interface {
	Scan(dest ...any) error
}

func scanBudgetPolicy(s budgetPolicyScanner) (*BudgetPolicyRow, error) {
	bp := &BudgetPolicyRow{}
	if err := s.Scan(
		&bp.ID, &bp.ScopeID, &bp.ProjectID, &bp.LimitCents, &bp.AccruedCents,
		&bp.PeriodStart, &bp.PeriodEnd, &bp.EnforcementAction,
		&bp.NotificationEmail, &bp.Status, &bp.CreatedBy,
		&bp.CreatedAt, &bp.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return bp, nil
}

// ── file-local error helpers ──────────────────────────────────────────────────
//
// Prefixed "isMetering" to avoid redeclaration conflicts with other db files.

func isMeteringNoRows(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no rows in result set")
}
