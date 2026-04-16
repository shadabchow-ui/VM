package db

// db.go — PostgreSQL access layer for the compute platform.
//
// Design: one Repo struct, one Pool interface, all repo methods on Repo.
// The Pool interface matches *pgxpool.Pool exactly so the real pool satisfies it
// without any adapter. Tests can inject a fake Pool.
//
// Source: IMPLEMENTATION_PLAN_V1 §A1 (PostgreSQL as single source of truth, pgx/v5).
//
// VM-P2E Slice 2 changes:
//   - HostRecord: added Generation int64 and DrainReason *string fields.
//   - GetHostByID, GetAvailableHosts: scan generation + drain_reason.
//   - UpdateHostStatus: nil-safe drainReason arg; sets updated_at.
//   - MarkHostDrained: new — gates draining→drained on zero active workload.
//   - DetachStoppedInstancesFromHost: now also sets updated_at on matched rows.
//   - CountActiveInstancesOnHost: unchanged, used by MarkHostDrained.
//
// VM-P2E Slice 3 changes:
//   - HostRecord: added ReasonCode *string and FenceRequired bool fields.
//   - GetHostByID, GetAvailableHosts: scan reason_code + fence_required.
//   - MarkHostDegraded: new — CAS transition to 'degraded' with reason_code.
//   - MarkHostUnhealthy: new — CAS transition to 'unhealthy' with reason_code;
//     sets fence_required=TRUE for ambiguous failure reason codes.
//   - ClearFenceRequired: new — operator/controller clears fence_required flag.
//   - GetFenceRequiredHosts: new — scan for hosts with fence_required=TRUE.
//   - ValidateHostTransition: new — guards illegal status transitions.
//
// VM-P2E Slice 4 changes:
//   - HostRecord: added RetiredAt *time.Time field (requires migration column).
//   - GetHostByID, GetAvailableHosts: scan retired_at.
//   - legalTransitions: added drained→retiring, fenced→retiring, retiring→retired,
//     drained→retired (direct admin path), unhealthy→retiring (for fenced-then-retire
//     without an explicit fencing controller landing before Slice 5).
//   - ReasonOperatorRetired: new constant for operator-initiated retirement.
//   - MarkHostRetiring: new — CAS transition to 'retiring'; gated on zero active workload.
//   - MarkHostRetired: new — CAS transition to 'retiring'→'retired'; sets retired_at.
//   - GetRetiredHosts: new — replacement-seam query returning all 'retired' hosts.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ── Pool interface ────────────────────────────────────────────────────────────

type CommandTag interface {
	RowsAffected() int64
}

type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

type Row interface {
	Scan(dest ...any) error
}

type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Close()
}

// ── Repo ─────────────────────────────────────────────────────────────────────

type Repo struct {
	pool Pool
}

func New(pool Pool) *Repo {
	return &Repo{pool: pool}
}

func DatabaseURL() string {
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		panic("DATABASE_URL is not set")
	}
	return u
}

func (r *Repo) Ping(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, "SELECT 1")
	if err != nil {
		return fmt.Errorf("db ping: %w", err)
	}
	return nil
}

// ── Domain errors ─────────────────────────────────────────────────────────────

var ErrHostNotFound = errors.New("host not found")
var ErrBootstrapTokenInvalid = errors.New("bootstrap token invalid, expired, or already used")
var ErrIllegalHostTransition = errors.New("illegal host status transition")

// ── Host reason codes ─────────────────────────────────────────────────────────
//
// These are the canonical machine-readable reason codes stored in hosts.reason_code.
// They describe why a host entered its current non-ready state.
//
// VM-P2E Slice 3: reason codes for degraded/unhealthy transitions.
// Future slices may add additional codes for retirement/fencing.
//
// Source: vm-13-03__research__ §"Host Health Signals and Fencing Decisions".
const (
	// ReasonAgentUnresponsive: heartbeat missed beyond the degraded threshold.
	// Sets fence_required=TRUE when escalated to unhealthy.
	ReasonAgentUnresponsive = "AGENT_UNRESPONSIVE"

	// ReasonAgentFailed: agent health probe returned an explicit error.
	// Does NOT set fence_required (agent failure is recoverable by restart).
	ReasonAgentFailed = "AGENT_FAILED"

	// ReasonStorageError: local storage I/O errors or read-only filesystem.
	// Does NOT set fence_required (storage issues don't indicate split-brain risk).
	ReasonStorageError = "STORAGE_ERROR"

	// ReasonHypervisorFailed: firecracker/hypervisor daemon is unresponsive.
	// Sets fence_required=TRUE when unhealthy — ambiguous whether VMs are still running.
	ReasonHypervisorFailed = "HYPERVISOR_FAILED"

	// ReasonNetworkUnreachable: data-plane network connectivity lost from control plane.
	// Sets fence_required=TRUE when unhealthy — split-brain risk if VMs are still running.
	ReasonNetworkUnreachable = "NETWORK_UNREACHABLE"

	// ReasonOperatorDegraded: operator manually marked the host degraded.
	// Does NOT set fence_required (operator-initiated, not an ambiguous failure).
	ReasonOperatorDegraded = "OPERATOR_DEGRADED"

	// ReasonOperatorUnhealthy: operator manually marked the host unhealthy.
	// Does NOT set fence_required (operator knows the host state explicitly).
	ReasonOperatorUnhealthy = "OPERATOR_UNHEALTHY"

	// ReasonOperatorRetired: operator explicitly retired the host.
	// Stored as reason_code on the retiring/retired transition so the audit log
	// shows why the host was removed from service.
	// Does NOT set fence_required (retirement is deliberate, not an ambiguous failure).
	// Source: vm-13-03__blueprint__ §"Emergency Retirement" operator procedure.
	ReasonOperatorRetired = "OPERATOR_RETIRED"
)

// fenceRequiredReasons is the set of reason codes that trigger fence_required=TRUE
// when a host transitions to 'unhealthy'. These are ambiguous failures where the
// control plane cannot confirm the host is powered off, so VMs may still be
// running — a split-brain risk if recovery automation were to start immediately.
//
// Source: vm-13-03__blueprint__ §"Fencing Decision Logic".
var fenceRequiredReasons = map[string]bool{
	ReasonAgentUnresponsive:  true,
	ReasonHypervisorFailed:   true,
	ReasonNetworkUnreachable: true,
}

// ── Host status transition rules ──────────────────────────────────────────────
//
// legalTransitions defines the set of valid (fromStatus → toStatus) pairs
// for host lifecycle state changes.
//
// Design rationale:
//   - Prevents silent corruption of host state by concurrent or incorrect actors.
//   - Transition rules are derived from vm-13-03__blueprint__ and research docs.
//   - Missing from this table = illegal; callers get ErrIllegalHostTransition.
//
// Allowed graph after Slice 4:
//
//	ready       → draining    (Slice 1: operator drain)
//	ready       → degraded    (Slice 3: health monitor detects issues)
//	ready       → unhealthy   (Slice 3: health monitor escalation, direct)
//	draining    → drained     (Slice 2: drain complete)
//	draining    → degraded    (Slice 3: health degrades during drain)
//	draining    → unhealthy   (Slice 3: health fails during drain)
//	drained     → ready       (post-maintenance reactivation seam)
//	drained     → degraded    (Slice 3: health degrades after drain)
//	drained     → retiring    (Slice 4: normal retirement path; requires zero active workload)
//	drained     → retired     (Slice 4: direct admin-only shortcut; narrow and explicit)
//	degraded    → ready       (Slice 3: recovery — transient issue resolved)
//	degraded    → unhealthy   (Slice 3: escalation — issue persists)
//	degraded    → draining    (Slice 3: operator decides to drain degraded host)
//	unhealthy   → degraded    (Slice 3: partial recovery signal)
//	unhealthy   → ready       (only via operator/explicit clearance after unhealthy)
//	unhealthy   → retiring    (Slice 4: emergency retirement for confirmed-bad host
//	                           without a full fencing controller; narrow admin path)
//	fenced      → retiring    (Slice 4: seam for fenced-then-retire; fencing controller
//	                           (Slice 5+) transitions fenced→retiring after STONITH)
//	retiring    → retired     (Slice 4: retirement completes after operator confirms)
//
// NOTE: the generation-checked CAS in UpdateHostStatus/MarkHostDegraded/
// MarkHostUnhealthy still applies — ValidateHostTransition is an additional
// guard, not a replacement for the generation check.
var legalTransitions = map[string]map[string]bool{
	"ready": {
		"draining":  true,
		"degraded":  true,
		"unhealthy": true,
	},
	"draining": {
		"drained":   true,
		"degraded":  true,
		"unhealthy": true,
	},
	"drained": {
		"ready":    true, // reactivation after maintenance
		"degraded": true,
		"retiring": true, // Slice 4: normal retirement path
		"retired":  true, // Slice 4: direct admin-only shortcut
	},
	"degraded": {
		"ready":     true, // transient issue resolved
		"unhealthy": true, // escalation
		"draining":  true, // operator-initiated drain on degraded host
	},
	"unhealthy": {
		"degraded": true, // partial recovery
		"ready":    true, // explicit operator clearance
		"retiring": true, // Slice 4: emergency retirement (confirmed-bad, no fencing controller yet)
	},
	"fenced": {
		"retiring": true, // Slice 4: seam for fencing-controller → retire path
	},
	"retiring": {
		"retired": true, // Slice 4: retirement completes
	},
}

// ValidateHostTransition returns ErrIllegalHostTransition if the fromStatus →
// toStatus pair is not in the legal transition table.
//
// Empty fromStatus (new host) is always allowed.
// This function is called by MarkHostDegraded and MarkHostUnhealthy before
// issuing the CAS UPDATE, so the DB never sees an illegal transition attempt.
//
// Source: vm-13-03__blueprint__ §"State Definitions & Transitions".
func ValidateHostTransition(fromStatus, toStatus string) error {
	if fromStatus == "" {
		return nil // new host, any initial status is fine
	}
	allowed, ok := legalTransitions[fromStatus]
	if !ok {
		return fmt.Errorf("%w: unknown fromStatus %q", ErrIllegalHostTransition, fromStatus)
	}
	if !allowed[toStatus] {
		return fmt.Errorf("%w: %s → %s", ErrIllegalHostTransition, fromStatus, toStatus)
	}
	return nil
}

// ── HostRecord ────────────────────────────────────────────────────────────────

// HostRecord is the DB row representation of a physical hypervisor host.
// Source: db/migrations/002_hosts.up.sql.
//
// VM-P2E Slice 1: Generation and DrainReason columns added via migration.
// VM-P2E Slice 2: Generation and DrainReason now scanned in all read paths.
// VM-P2E Slice 3: ReasonCode and FenceRequired added via migration.
type HostRecord struct {
	ID               string
	AvailabilityZone string
	Status           string
	Generation       int64   // optimistic concurrency counter; incremented on every status CAS
	DrainReason      *string // operator-supplied drain reason; nil if not set
	ReasonCode       *string // machine-readable reason for current non-ready state; nil if ready
	FenceRequired    bool       // TRUE when fence_required=TRUE: fencing must complete before recovery
	RetiredAt        *time.Time // wall-clock time host transitioned to 'retired'; nil until then
	TotalCPU         int
	TotalMemoryMB    int
	TotalDiskGB      int
	UsedCPU          int
	UsedMemoryMB     int
	UsedDiskGB       int
	AgentVersion     string
	LastHeartbeatAt  *time.Time
	RegisteredAt     time.Time
	UpdatedAt        time.Time
}

func (h *HostRecord) CanFit(cpuCores, memoryMB, diskGB int) bool {
	return h.Status == "ready" &&
		(h.TotalCPU-h.UsedCPU) >= cpuCores &&
		(h.TotalMemoryMB-h.UsedMemoryMB) >= memoryMB &&
		(h.TotalDiskGB-h.UsedDiskGB) >= diskGB
}

// IsSchedulable returns true only for hosts in 'ready' status.
// All other statuses (draining, drained, degraded, unhealthy, fenced, retired, etc.)
// are unschedulable. This mirrors the scheduler's CanFit status check.
func (h *HostRecord) IsSchedulable() bool {
	return h.Status == "ready"
}

// ── Host repo methods ─────────────────────────────────────────────────────────

func (r *Repo) UpsertHost(ctx context.Context, rec *HostRecord) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO hosts (
			id, availability_zone, status,
			total_cpu, total_memory_mb, total_disk_gb,
			used_cpu, used_memory_mb, used_disk_gb,
			agent_version, registered_at, updated_at
		) VALUES ($1, $2, 'ready', $3, $4, $5, 0, 0, 0, $6, NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET
			availability_zone = EXCLUDED.availability_zone,
			total_cpu         = EXCLUDED.total_cpu,
			total_memory_mb   = EXCLUDED.total_memory_mb,
			total_disk_gb     = EXCLUDED.total_disk_gb,
			agent_version     = EXCLUDED.agent_version,
			status            = 'ready',
			updated_at        = NOW()
	`,
		rec.ID, rec.AvailabilityZone,
		rec.TotalCPU, rec.TotalMemoryMB, rec.TotalDiskGB,
		rec.AgentVersion,
	)
	return err
}

func (r *Repo) UpdateHeartbeat(ctx context.Context, hostID string, usedCPU, usedMemMB, usedDiskGB int, agentVersion string) error {
	now := time.Now().UTC()
	tag, err := r.pool.Exec(ctx, `
		UPDATE hosts
		SET used_cpu          = $2,
		    used_memory_mb    = $3,
		    used_disk_gb      = $4,
		    agent_version     = $5,
		    last_heartbeat_at = $6,
		    updated_at        = $6
		WHERE id = $1
	`, hostID, usedCPU, usedMemMB, usedDiskGB, agentVersion, now)
	if err != nil {
		return fmt.Errorf("UpdateHeartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateHeartbeat host_id=%s: %w", hostID, ErrHostNotFound)
	}
	return nil
}

// GetAvailableHosts returns all ready hosts with a recent heartbeat.
// VM-P2E Slice 2: scans generation and drain_reason alongside existing columns.
// VM-P2E Slice 3: scans reason_code and fence_required.
// VM-P2E Slice 4: scans retired_at.
func (r *Repo) GetAvailableHosts(ctx context.Context) ([]*HostRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, availability_zone, status,
		       generation, drain_reason, reason_code, fence_required, retired_at,
		       total_cpu, total_memory_mb, total_disk_gb,
		       used_cpu, used_memory_mb, used_disk_gb,
		       agent_version, last_heartbeat_at, registered_at, updated_at
		FROM hosts
		WHERE status = 'ready'
		  AND last_heartbeat_at > NOW() - INTERVAL '90 seconds'
		ORDER BY (total_cpu - used_cpu) DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("GetAvailableHosts: %w", err)
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
			return nil, fmt.Errorf("GetAvailableHosts scan: %w", err)
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

// GetHostByID fetches a single host record regardless of status.
// Returns ErrHostNotFound if absent.
// VM-P2E Slice 2: scans generation and drain_reason.
// VM-P2E Slice 3: scans reason_code and fence_required.
// VM-P2E Slice 4: scans retired_at.
func (r *Repo) GetHostByID(ctx context.Context, hostID string) (*HostRecord, error) {
	h := &HostRecord{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, availability_zone, status,
		       generation, drain_reason, reason_code, fence_required, retired_at,
		       total_cpu, total_memory_mb, total_disk_gb,
		       used_cpu, used_memory_mb, used_disk_gb,
		       agent_version, last_heartbeat_at, registered_at, updated_at
		FROM hosts WHERE id = $1
	`, hostID).Scan(
		&h.ID, &h.AvailabilityZone, &h.Status,
		&h.Generation, &h.DrainReason, &h.ReasonCode, &h.FenceRequired, &h.RetiredAt,
		&h.TotalCPU, &h.TotalMemoryMB, &h.TotalDiskGB,
		&h.UsedCPU, &h.UsedMemoryMB, &h.UsedDiskGB,
		&h.AgentVersion, &h.LastHeartbeatAt, &h.RegisteredAt, &h.UpdatedAt,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, fmt.Errorf("GetHostByID %s: %w", hostID, ErrHostNotFound)
		}
		return nil, fmt.Errorf("GetHostByID: %w", err)
	}
	return h, nil
}

// ── Bootstrap token methods ───────────────────────────────────────────────────

func (r *Repo) ConsumeBootstrapToken(ctx context.Context, tokenHash string) (string, error) {
	var hostID string
	err := r.pool.QueryRow(ctx, `
		UPDATE bootstrap_tokens
		SET used = TRUE
		WHERE token_hash = $1
		  AND used       = FALSE
		  AND expires_at > NOW()
		RETURNING host_id
	`, tokenHash).Scan(&hostID)
	if err != nil {
		return "", ErrBootstrapTokenInvalid
	}
	return hostID, nil
}

func (r *Repo) InsertBootstrapToken(ctx context.Context, tokenHash, hostID string, expiresAt time.Time) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO bootstrap_tokens (token_hash, host_id, expires_at, used, created_at)
		VALUES ($1, $2, $3, FALSE, NOW())
		ON CONFLICT (host_id) DO UPDATE SET
			token_hash = EXCLUDED.token_hash,
			expires_at = EXCLUDED.expires_at,
			used       = FALSE,
			created_at = NOW()
	`, tokenHash, hostID, expiresAt)
	return err
}

// ── Host lifecycle state methods ──────────────────────────────────────────────

// UpdateHostStatus performs a generation-checked CAS status transition.
//
// Returns (true, nil) when the row was updated.
// Returns (false, nil) when the CAS failed (generation mismatch or host not found).
//
// drainReason: pass empty string to clear the column (stored as SQL NULL).
//
// Note: UpdateHostStatus does NOT enforce ValidateHostTransition — it is a
// low-level CAS. Callers (DrainHost, MarkHostDegraded, MarkHostUnhealthy) are
// responsible for transition validation before calling this method.
//
// Source: vm-13-03__blueprint__ §implementation_decisions generation enforcement.
func (r *Repo) UpdateHostStatus(ctx context.Context, hostID string, fromGeneration int64, newStatus, drainReason string) (bool, error) {
	// Store drain_reason as NULL when empty so the column stays clean for non-drain transitions.
	var drainReasonVal interface{}
	if drainReason != "" {
		drainReasonVal = drainReason
	}
	res, err := r.pool.Exec(ctx, `
		UPDATE hosts
		SET status       = $2,
		    drain_reason = $3,
		    generation   = generation + 1,
		    updated_at   = NOW()
		WHERE id = $1 AND generation = $4
	`, hostID, newStatus, drainReasonVal, fromGeneration)
	if err != nil {
		return false, err
	}
	return res.RowsAffected() == 1, nil
}

// MarkHostDrained attempts to transition a host from draining → drained.
//
// The transition is only performed when no active VM workload remains on the host.
// Active states: requested, provisioning, running, stopping, rebooting, deleting.
//
// Return values:
//   - (n>0, false, nil): n active instances remain; drain not complete yet.
//   - (0, false, nil):   generation mismatch, host not in draining state, or already drained.
//   - (0, true,  nil):   transition succeeded; host is now drained.
//   - (0, false, err):   DB error.
//
// Idempotency: calling this again after a successful transition returns (0, false, nil)
// because the WHERE clause requires status='draining', which will no longer match.
//
// Source: vm-13-03__blueprint__ §core_contracts "Host State Atomicity",
//         §interaction_or_ops_contract "Operator confirms drain complete".
func (r *Repo) MarkHostDrained(ctx context.Context, hostID string, fromGeneration int64) (activeCount int, updated bool, err error) {
	// Step 1: count active workload. Do this before the CAS attempt so we
	// can return an informative count to the caller.
	n, err := r.CountActiveInstancesOnHost(ctx, hostID)
	if err != nil {
		return 0, false, fmt.Errorf("MarkHostDrained count: %w", err)
	}
	if n > 0 {
		// Active workload blocks the transition — caller should retry later.
		return n, false, nil
	}

	// Step 2: CAS draining → drained. The generation guard prevents races
	// with concurrent drain-complete attempts or other state transitions.
	// drain_reason is preserved so the original reason for draining remains observable.
	res, err := r.pool.Exec(ctx, `
		UPDATE hosts
		SET status     = 'drained',
		    generation = generation + 1,
		    updated_at = NOW()
		WHERE id         = $1
		  AND generation = $2
		  AND status     = 'draining'
	`, hostID, fromGeneration)
	if err != nil {
		return 0, false, fmt.Errorf("MarkHostDrained update: %w", err)
	}
	return 0, res.RowsAffected() == 1, nil
}

// MarkHostDegraded transitions a host to 'degraded' with a reason code.
//
// Valid fromStatuses: ready, draining, drained (see legalTransitions).
// The transition is generation-checked to prevent races.
//
// reasonCode must be one of the ReasonXxx constants defined in this package.
// An unrecognized reason code is stored as-is (the DB does not enforce an enum
// for extensibility); callers should use the defined constants.
//
// Returns (true, nil) on success. Returns (false, nil) on CAS failure.
// Returns (false, err) on DB error or illegal transition.
//
// Source: vm-13-03__blueprint__ §implementation_decisions
//         "Introduce a DEGRADED state to precede the terminal UNHEALTHY state".
func (r *Repo) MarkHostDegraded(ctx context.Context, hostID string, fromGeneration int64, fromStatus, reasonCode string) (bool, error) {
	if err := ValidateHostTransition(fromStatus, "degraded"); err != nil {
		return false, err
	}

	var reasonVal interface{}
	if reasonCode != "" {
		reasonVal = reasonCode
	}

	res, err := r.pool.Exec(ctx, `
		UPDATE hosts
		SET status       = 'degraded',
		    reason_code  = $3,
		    generation   = generation + 1,
		    updated_at   = NOW()
		WHERE id         = $1
		  AND generation = $2
		  AND status     = $4
	`, hostID, fromGeneration, reasonVal, fromStatus)
	if err != nil {
		return false, fmt.Errorf("MarkHostDegraded: %w", err)
	}
	return res.RowsAffected() == 1, nil
}

// MarkHostUnhealthy transitions a host to 'unhealthy' with a reason code.
//
// Valid fromStatuses: ready, draining, degraded (see legalTransitions).
// The transition is generation-checked.
//
// fence_required is set to TRUE when reasonCode is in the fenceRequiredReasons
// set (AGENT_UNRESPONSIVE, HYPERVISOR_FAILED, NETWORK_UNREACHABLE). For all
// other reason codes, fence_required remains FALSE.
//
// This is the "fencing groundwork seam": by setting fence_required=TRUE in the
// DB, the record is observable and testable. A future fencing controller (Slice 4+)
// will scan for fence_required=TRUE hosts and execute STONITH before clearing the
// flag. Recovery automation must NOT proceed while fence_required=TRUE.
//
// Returns (fenceRequired bool, updated bool, err error).
//   - (true/false, true,  nil): transition succeeded.
//   - (false,      false, nil): CAS failure (generation mismatch or wrong fromStatus).
//   - (false,      false, err): DB error or illegal transition.
//
// Source: vm-13-03__blueprint__ §"Fencing Decision Logic",
//         §"Fencing Controller" (fence_required flag is the seam for Slice 4).
func (r *Repo) MarkHostUnhealthy(ctx context.Context, hostID string, fromGeneration int64, fromStatus, reasonCode string) (fenceRequired bool, updated bool, err error) {
	if err := ValidateHostTransition(fromStatus, "unhealthy"); err != nil {
		return false, false, err
	}

	fenceRequired = fenceRequiredReasons[reasonCode]

	var reasonVal interface{}
	if reasonCode != "" {
		reasonVal = reasonCode
	}

	res, dbErr := r.pool.Exec(ctx, `
		UPDATE hosts
		SET status         = 'unhealthy',
		    reason_code    = $3,
		    fence_required = $4,
		    generation     = generation + 1,
		    updated_at     = NOW()
		WHERE id         = $1
		  AND generation = $2
		  AND status     = $5
	`, hostID, fromGeneration, reasonVal, fenceRequired, fromStatus)
	if dbErr != nil {
		return false, false, fmt.Errorf("MarkHostUnhealthy: %w", dbErr)
	}
	if res.RowsAffected() == 0 {
		// CAS failed — nothing was written to DB. Do not report fenceRequired=true
		// since no fence_required flag was actually set.
		return false, false, nil
	}
	return fenceRequired, true, nil
}

// ClearFenceRequired clears fence_required on a host.
//
// This is called by an operator or (in Slice 4+) by the fencing controller
// after confirming the host has been isolated (STONITH complete).
//
// The call is generation-checked. The host status is NOT changed by this method;
// status transitions (e.g., unhealthy → fenced) are separate CAS operations.
//
// Returns (true, nil) when the flag was cleared.
// Returns (false, nil) on CAS failure.
//
// Source: vm-13-03__blueprint__ §"Fencing Controller" (seam for Slice 4+).
func (r *Repo) ClearFenceRequired(ctx context.Context, hostID string, fromGeneration int64) (bool, error) {
	res, err := r.pool.Exec(ctx, `
		UPDATE hosts
		SET fence_required = FALSE,
		    generation     = generation + 1,
		    updated_at     = NOW()
		WHERE id         = $1
		  AND generation = $2
		  AND fence_required = TRUE
	`, hostID, fromGeneration)
	if err != nil {
		return false, fmt.Errorf("ClearFenceRequired: %w", err)
	}
	return res.RowsAffected() == 1, nil
}

// GetFenceRequiredHosts returns all hosts with fence_required=TRUE.
//
// This is the observable query surface for the fencing groundwork seam.
// Operator tooling and the future fencing controller (Slice 4+) use this to
// find hosts that need STONITH before recovery automation may proceed.
//
// Source: vm-13-03__blueprint__ §"Fencing Controller".
func (r *Repo) GetFenceRequiredHosts(ctx context.Context) ([]*HostRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, availability_zone, status,
		       generation, drain_reason, reason_code, fence_required, retired_at,
		       total_cpu, total_memory_mb, total_disk_gb,
		       used_cpu, used_memory_mb, used_disk_gb,
		       agent_version, last_heartbeat_at, registered_at, updated_at
		FROM hosts
		WHERE fence_required = TRUE
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("GetFenceRequiredHosts: %w", err)
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
			return nil, fmt.Errorf("GetFenceRequiredHosts scan: %w", err)
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

// DetachStoppedInstancesFromHost clears host_id on stopped instances tied to
// this host so they do not block drain completion.
//
// Idempotent: safe to call repeatedly. Already-detached rows are unaffected.
//
// Race note: instances transitioning from stopped → provisioning concurrently
// will have a non-stopped status and will NOT be detached. The worker start
// handler is responsible for handling re-placement on draining hosts.
//
// Source: vm-13-03__skill__ §instructions stopped-VM re-association.
func (r *Repo) DetachStoppedInstancesFromHost(ctx context.Context, hostID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE instances
		SET host_id    = NULL,
		    updated_at = NOW()
		WHERE host_id = $1
		  AND status  = 'stopped'
	`, hostID)
	return err
}

// CountActiveInstancesOnHost returns the number of instances in active lifecycle
// states on the given host. Used by MarkHostDrained to gate the transition.
func (r *Repo) CountActiveInstancesOnHost(ctx context.Context, hostID string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM instances
		WHERE host_id = $1
		  AND status IN ('requested','provisioning','running','stopping','rebooting','deleting')
	`, hostID).Scan(&n)
	return n, err
}

// ── VM-P2E Slice 4: Retirement lifecycle methods ──────────────────────────────

// MarkHostRetiring transitions a host to 'retiring'.
//
// Valid fromStatuses: drained, fenced, unhealthy (see legalTransitions).
// The transition is generation-checked (CAS).
//
// The transition is BLOCKED (returns activeCount>0, false, nil) if any active
// VM workload remains on the host. Active states: requested, provisioning,
// running, stopping, rebooting, deleting.
//
// Design: mirrors MarkHostDrained's workload-gate semantics so callers have a
// consistent pattern. Retirement requires a fully empty host for safety.
//
// reasonCode should be ReasonOperatorRetired or a caller-supplied code.
// An empty reasonCode is stored as NULL.
//
// Return values:
//   - (n>0, false, nil): n active instances remain; retirement blocked.
//   - (0, false, nil):   CAS failed (generation mismatch, wrong status, host missing).
//   - (0, true,  nil):   transition succeeded; host is now 'retiring'.
//   - (0, false, err):   DB error or illegal transition.
//
// Source: vm-13-03__blueprint__ §"Emergency Retirement" and
//         §core_contracts "Stopped Instance Ephemerality".
func (r *Repo) MarkHostRetiring(ctx context.Context, hostID string, fromGeneration int64, fromStatus, reasonCode string) (activeCount int, updated bool, err error) {
	if err := ValidateHostTransition(fromStatus, "retiring"); err != nil {
		return 0, false, err
	}

	// Gate on zero active workload — a host must be empty to retire safely.
	n, err := r.CountActiveInstancesOnHost(ctx, hostID)
	if err != nil {
		return 0, false, fmt.Errorf("MarkHostRetiring count: %w", err)
	}
	if n > 0 {
		return n, false, nil
	}

	var reasonVal interface{}
	if reasonCode != "" {
		reasonVal = reasonCode
	}

	res, dbErr := r.pool.Exec(ctx, `
		UPDATE hosts
		SET status      = 'retiring',
		    reason_code = $3,
		    generation  = generation + 1,
		    updated_at  = NOW()
		WHERE id         = $1
		  AND generation = $2
		  AND status     = $4
	`, hostID, fromGeneration, reasonVal, fromStatus)
	if dbErr != nil {
		return 0, false, fmt.Errorf("MarkHostRetiring: %w", dbErr)
	}
	return 0, res.RowsAffected() == 1, nil
}

// MarkHostRetired transitions a host from 'retiring' to 'retired'.
//
// Sets retired_at to NOW() — this timestamp is the replacement-seam anchor:
// Slice 5+ capacity managers will query retired hosts ordered by retired_at
// to know which capacity holes are oldest and need backfilling first.
//
// The transition is generation-checked (CAS) and requires status='retiring'.
// This is intentional: callers must go through MarkHostRetiring first unless
// they use the direct drained→retired path via UpdateHostStatus (admin shortcut).
//
// Returns (true, nil) on success.
// Returns (false, nil) on CAS failure (wrong generation or not in 'retiring').
// Returns (false, err) on DB error.
//
// Source: vm-13-03__blueprint__ §"Operator Procedures for Maintenance and
//         Emergency Retirement", §"RETIRED state — terminal".
func (r *Repo) MarkHostRetired(ctx context.Context, hostID string, fromGeneration int64) (bool, error) {
	res, err := r.pool.Exec(ctx, `
		UPDATE hosts
		SET status      = 'retired',
		    retired_at  = NOW(),
		    generation  = generation + 1,
		    updated_at  = NOW()
		WHERE id         = $1
		  AND generation = $2
		  AND status     = 'retiring'
	`, hostID, fromGeneration)
	if err != nil {
		return false, fmt.Errorf("MarkHostRetired: %w", err)
	}
	return res.RowsAffected() == 1, nil
}

// GetRetiredHosts returns all hosts with status='retired', ordered by retired_at DESC.
//
// This is the replacement-seam query for Slice 5+ capacity planning.
// A Slice 5 replacement orchestrator queries this surface to discover capacity
// holes (retired hosts that have not yet been replaced) and trigger bare-metal
// provisioning. Results are ordered oldest-retired-first so the orchestrator
// can prioritize the longest-standing capacity deficits.
//
// This is a pure read — no state mutations.
//
// Source: vm-13-03__blueprint__ §"Capacity Rebalancing and Spare-Capacity Strategy",
//         §components "Capacity Manager" (Slice 5+ seam).
func (r *Repo) GetRetiredHosts(ctx context.Context) ([]*HostRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, availability_zone, status,
		       generation, drain_reason, reason_code, fence_required, retired_at,
		       total_cpu, total_memory_mb, total_disk_gb,
		       used_cpu, used_memory_mb, used_disk_gb,
		       agent_version, last_heartbeat_at, registered_at, updated_at
		FROM hosts
		WHERE status = 'retired'
		ORDER BY retired_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("GetRetiredHosts: %w", err)
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
			return nil, fmt.Errorf("GetRetiredHosts scan: %w", err)
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}
