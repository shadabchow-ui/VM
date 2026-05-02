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
	// records intent, not just a status change.
	// Source: vm-13-03__blueprint__ §components "Fleet Management Service".
	ReasonOperatorRetired = "OPERATOR_RETIRED"

	// ReasonHostStale: host heartbeat has not been received within the
	// degraded threshold (heartbeat staleness window > heartbeatInterval * degradedMultiplier).
	// Stored as reason_code when a host transitions from 'ready' to 'degraded'
	// due to stale heartbeat detection. Operator-visible so tooling can
	// distinguish stale hosts from explicitly-degraded hosts.
	ReasonHostStale = "HOST_STALE"

	// ambiguousFenceReasonCodes is the set of reason codes that cause
	// fence_required=TRUE when a host transitions to 'unhealthy'.
	// These codes indicate the control plane cannot determine whether
	// running VMs are still alive on the host, so STONITH isolation
	// must complete before recovery automation may proceed.
)

// ambiguousFenceReasonCodes is the set of reason codes that cause
// fence_required=TRUE when a host transitions to 'unhealthy'.
// These codes indicate the control plane cannot determine whether
// running VMs are still alive on the host, so STONITH isolation
// must complete before recovery automation may proceed.
var ambiguousFenceReasonCodes = map[string]bool{
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
	Generation       int64      // optimistic concurrency counter; incremented on every status CAS
	DrainReason      *string    // operator-supplied drain reason; nil if not set
	ReasonCode       *string    // machine-readable reason for current non-ready state; nil if ready
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

// SwitchToHeartbeat stores the host's boot_id and returns the previous boot_id
// so the caller can detect a host reboot (boot_id change = host restarted).
// If the boot_id changed and the previous was non-empty, the host has rebooted
// and all VMs that were running on it are presumed lost.
//
// VM Job 9: boot_id change detection is a safety signal — a reboot event means
// any previously-running VMs on this host are gone. The reconciler must handle
// this by classifying those VMs as failed (host reboot = unambiguous failure).
func (r *Repo) UpdateHeartbeatBootID(ctx context.Context, hostID, bootID string) (previousBootID string, err error) {
	row := r.pool.QueryRow(ctx, `SELECT boot_id FROM hosts WHERE id = $1`, hostID)
	if err := row.Scan(&previousBootID); err != nil {
		return "", fmt.Errorf("UpdateHeartbeatBootID select: %w", err)
	}
	_, err = r.pool.Exec(ctx, `UPDATE hosts SET boot_id = $2, updated_at = NOW() WHERE id = $1`, hostID, bootID)
	if err != nil {
		return previousBootID, fmt.Errorf("UpdateHeartbeatBootID update: %w", err)
	}
	return previousBootID, nil
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

// GetStaleHosts returns ready hosts whose heartbeat is older than the staleness threshold.
//
// Staleness threshold: 90 seconds (3 × heartbeatInterval of 30s).
// These hosts are still status=ready but have silently stopped heartbeating.
// They are excluded from GetAvailableHosts by the heartbeat timestamp filter;
// this query exposes them explicitly so operators can see which hosts need attention.
//
// This is a pure read — no status transitions are performed.
// The operator explicitly marks stale hosts degraded via POST .../degraded.
//
// Source: VM Job 9 — host fleet operations + failure recovery gate.
func (r *Repo) GetStaleHosts(ctx context.Context) ([]*HostRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, availability_zone, status,
		       generation, drain_reason, reason_code, fence_required, retired_at,
		       total_cpu, total_memory_mb, total_disk_gb,
		       used_cpu, used_memory_mb, used_disk_gb,
		       agent_version, last_heartbeat_at, registered_at, updated_at
		FROM hosts
		WHERE status = 'ready'
		  AND last_heartbeat_at <= NOW() - INTERVAL '90 seconds'
		ORDER BY last_heartbeat_at ASC NULLS FIRST
	`)
	if err != nil {
		return nil, fmt.Errorf("GetStaleHosts: %w", err)
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
			return nil, fmt.Errorf("GetStaleHosts scan: %w", err)
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
//
//	§interaction_or_ops_contract "Operator confirms drain complete".
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
//
//	"Introduce a DEGRADED state to precede the terminal UNHEALTHY state".
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
// fence_required is set to TRUE when reasonCode is in the ambiguousFenceReasonCodes
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
//
//	§"Fencing Controller" (fence_required flag is the seam for Slice 4).
func (r *Repo) MarkHostUnhealthy(ctx context.Context, hostID string, fromGeneration int64, fromStatus, reasonCode string) (fenceRequired bool, updated bool, err error) {
	if err := ValidateHostTransition(fromStatus, "unhealthy"); err != nil {
		return false, false, err
	}

	fenceRequired = ambiguousFenceReasonCodes[reasonCode]

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
//
//	§core_contracts "Stopped Instance Ephemerality".
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
//
//	Emergency Retirement", §"RETIRED state — terminal".
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
//
//	§components "Capacity Manager" (Slice 5+ seam).
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

// ── VM-P2E Slice 5: Maintenance campaign orchestration ────────────────────────
//
// A maintenance campaign is a bounded, operator-defined batch of hosts that
// should be drained or retired in a controlled sequence with explicit
// blast-radius limits.
//
// Design:
//   - Campaigns are persisted in maintenance_campaigns. One row per campaign.
//   - max_parallel is immutable after creation — blast-radius intent is locked.
//   - Status transitions: pending → running → completed|cancelled|paused.
//   - Slice 6 recovery seam: failed_host_ids list is observable by a future
//     recovery actor without requiring a schema change.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator",
//         §interaction_or_ops_contract "Operator initiates a fleet-wide kernel update".

// MaxCampaignParallel is the hard upper bound on max_parallel for any campaign.
// Blast-radius rule: no campaign may act on more than this many hosts at once.
// Operators set max_parallel ≤ MaxCampaignParallel; values above are rejected.
//
// This value is deliberately conservative for Phase 1. Slice 6+ may allow it
// to be per-campaign-configurable up to a fleet-size-relative ceiling.
const MaxCampaignParallel = 10

// ErrCampaignNotFound is returned by GetCampaignByID when the campaign is absent.
var ErrCampaignNotFound = fmt.Errorf("campaign not found")

// ErrBlastRadiusExceeded is returned when max_parallel would exceed MaxCampaignParallel.
var ErrBlastRadiusExceeded = fmt.Errorf("max_parallel exceeds blast-radius limit of %d", MaxCampaignParallel)

// ErrCampaignNoTargets is returned when a campaign is created with zero target hosts.
var ErrCampaignNoTargets = fmt.Errorf("campaign must have at least one target host")

// CampaignRecord is the DB row representation of a maintenance_campaigns row.
type CampaignRecord struct {
	ID               string
	CampaignReason   string
	TargetHostIDs    []string // full list of hosts submitted; immutable after creation
	CompletedHostIDs []string // hosts whose action succeeded
	FailedHostIDs    []string // hosts whose action failed; Slice 6 recovery seam
	MaxParallel      int      // blast-radius limit; immutable after creation
	Status           string   // pending | running | paused | completed | cancelled
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// InFlightCount returns the number of hosts currently being acted on.
// Calculated from TargetHostIDs minus completed and failed host counts.
// A host is "in-flight" once the campaign has advanced past it but before
// it appears in completed or failed lists.
func (c *CampaignRecord) InFlightCount() int {
	return len(c.TargetHostIDs) - len(c.CompletedHostIDs) - len(c.FailedHostIDs)
}

// IsTerminal returns true when the campaign is in a terminal state
// (completed or cancelled). Terminal campaigns cannot be advanced.
func (c *CampaignRecord) IsTerminal() bool {
	return c.Status == "completed" || c.Status == "cancelled"
}

// NextHosts returns up to n host IDs that have not yet been completed or failed.
// Used by the advance logic to find the next batch to act on.
func (c *CampaignRecord) NextHosts(n int) []string {
	done := make(map[string]bool, len(c.CompletedHostIDs)+len(c.FailedHostIDs))
	for _, h := range c.CompletedHostIDs {
		done[h] = true
	}
	for _, h := range c.FailedHostIDs {
		done[h] = true
	}
	var next []string
	for _, h := range c.TargetHostIDs {
		if !done[h] {
			next = append(next, h)
			if len(next) >= n {
				break
			}
		}
	}
	return next
}

// ── Campaign repo methods ─────────────────────────────────────────────────────

// CreateCampaign inserts a new maintenance campaign record.
//
// Validations (enforced here, not just at the handler layer):
//   - len(targetHostIDs) >= 1
//   - maxParallel >= 1
//   - maxParallel <= MaxCampaignParallel (blast-radius hard limit)
//
// The campaign is created in 'pending' status. The caller advances it via
// UpdateCampaignStatus or AdvanceCampaignProgress.
//
// id must be a caller-generated unique ID (e.g. UUID4). The DB has a PRIMARY KEY
// constraint so duplicate IDs return a DB error.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (r *Repo) CreateCampaign(ctx context.Context, id, reason string, targetHostIDs []string, maxParallel int) (*CampaignRecord, error) {
	if len(targetHostIDs) == 0 {
		return nil, ErrCampaignNoTargets
	}
	if maxParallel < 1 {
		maxParallel = 1
	}
	if maxParallel > MaxCampaignParallel {
		return nil, ErrBlastRadiusExceeded
	}

	_, err := r.pool.Exec(ctx, `
		INSERT INTO maintenance_campaigns (
			id, campaign_reason, target_host_ids,
			completed_host_ids, failed_host_ids,
			max_parallel, status, created_at, updated_at
		) VALUES ($1, $2, $3, '{}', '{}', $4, 'pending', NOW(), NOW())
	`, id, reason, targetHostIDs, maxParallel)
	if err != nil {
		return nil, fmt.Errorf("CreateCampaign: %w", err)
	}
	return r.GetCampaignByID(ctx, id)
}

// GetCampaignByID fetches a single campaign record by ID.
// Returns ErrCampaignNotFound when absent.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (r *Repo) GetCampaignByID(ctx context.Context, id string) (*CampaignRecord, error) {
	c := &CampaignRecord{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, campaign_reason,
		       target_host_ids, completed_host_ids, failed_host_ids,
		       max_parallel, status, created_at, updated_at
		FROM maintenance_campaigns
		WHERE id = $1
	`, id).Scan(
		&c.ID, &c.CampaignReason,
		&c.TargetHostIDs, &c.CompletedHostIDs, &c.FailedHostIDs,
		&c.MaxParallel, &c.Status, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, fmt.Errorf("GetCampaignByID %s: %w", id, ErrCampaignNotFound)
		}
		return nil, fmt.Errorf("GetCampaignByID: %w", err)
	}
	return c, nil
}

// ListCampaigns returns campaigns filtered by the given status values.
// Pass nil or empty slice to return all campaigns.
// Results are ordered by created_at DESC (newest first).
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator" observable surface.
func (r *Repo) ListCampaigns(ctx context.Context, statuses []string) ([]*CampaignRecord, error) {
	var (
		rows Rows
		err  error
	)
	if len(statuses) == 0 {
		rows, err = r.pool.Query(ctx, `
			SELECT id, campaign_reason,
			       target_host_ids, completed_host_ids, failed_host_ids,
			       max_parallel, status, created_at, updated_at
			FROM maintenance_campaigns
			ORDER BY created_at DESC
		`)
	} else {
		rows, err = r.pool.Query(ctx, `
			SELECT id, campaign_reason,
			       target_host_ids, completed_host_ids, failed_host_ids,
			       max_parallel, status, created_at, updated_at
			FROM maintenance_campaigns
			WHERE status = ANY($1)
			ORDER BY created_at DESC
		`, statuses)
	}
	if err != nil {
		return nil, fmt.Errorf("ListCampaigns: %w", err)
	}
	defer rows.Close()

	var campaigns []*CampaignRecord
	for rows.Next() {
		c := &CampaignRecord{}
		if err := rows.Scan(
			&c.ID, &c.CampaignReason,
			&c.TargetHostIDs, &c.CompletedHostIDs, &c.FailedHostIDs,
			&c.MaxParallel, &c.Status, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListCampaigns scan: %w", err)
		}
		campaigns = append(campaigns, c)
	}
	return campaigns, rows.Err()
}

// UpdateCampaignStatus updates the campaign status.
//
// Valid transitions:
//
//	pending   → running   (first advance)
//	running   → paused    (operator pause)
//	paused    → running   (operator resume)
//	running   → completed (all hosts done)
//	running   → cancelled (operator cancel)
//	paused    → cancelled (operator cancel while paused)
//
// Returns (true, nil) on success.
// Returns (false, nil) when the campaign was not found or status unchanged.
// Returns (false, err) on DB error.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (r *Repo) UpdateCampaignStatus(ctx context.Context, id, newStatus string) (bool, error) {
	res, err := r.pool.Exec(ctx, `
		UPDATE maintenance_campaigns
		SET status     = $2,
		    updated_at = NOW()
		WHERE id = $1
		  AND status != $2
	`, id, newStatus)
	if err != nil {
		return false, fmt.Errorf("UpdateCampaignStatus: %w", err)
	}
	return res.RowsAffected() == 1, nil
}

// AdvanceCampaignProgress records a host as completed or failed within a campaign.
//
// hostOutcome must be "completed" or "failed".
// The host is appended to the corresponding array column.
// updated_at is set to NOW().
//
// After the append, if all target hosts have been actioned (completed+failed ==
// target), the campaign status is set to 'completed' in the same call.
//
// This is NOT a blast-radius check — that is done by the handler before
// calling DrainHost. This method only records the outcome.
//
// Returns (true, nil) on success.
// Returns (false, nil) when the campaign is not found or hostID is not in target_host_ids.
// Returns (false, err) on DB error.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator" progress tracking.
func (r *Repo) AdvanceCampaignProgress(ctx context.Context, campaignID, hostID, hostOutcome string) (bool, error) {
	var colName string
	switch hostOutcome {
	case "completed":
		colName = "completed_host_ids"
	case "failed":
		colName = "failed_host_ids"
	default:
		return false, fmt.Errorf("AdvanceCampaignProgress: unknown outcome %q", hostOutcome)
	}

	// Append the host to the outcome array.
	// Use array_append — idempotent-ish (duplicates are benign for observability).
	// Only update if the campaign is not already terminal.
	query := fmt.Sprintf(`
		UPDATE maintenance_campaigns
		SET %s      = array_append(%s, $2),
		    updated_at = NOW()
		WHERE id     = $1
		  AND status NOT IN ('completed', 'cancelled')
		  AND $2 = ANY(target_host_ids)
	`, colName, colName)
	res, err := r.pool.Exec(ctx, query, campaignID, hostID)
	if err != nil {
		return false, fmt.Errorf("AdvanceCampaignProgress append: %w", err)
	}
	if res.RowsAffected() == 0 {
		return false, nil
	}

	// Auto-complete the campaign when all hosts are accounted for.
	// This is a best-effort update; callers may also poll status via GetCampaignByID.
	_, _ = r.pool.Exec(ctx, `
		UPDATE maintenance_campaigns
		SET status     = 'completed',
		    updated_at = NOW()
		WHERE id = $1
		  AND status = 'running'
		  AND array_length(completed_host_ids, 1) + array_length(failed_host_ids, 1)
		      >= array_length(target_host_ids, 1)
	`, campaignID)

	return true, nil
}
