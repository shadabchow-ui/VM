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

// ── HostRecord ────────────────────────────────────────────────────────────────

// HostRecord is the DB row representation of a physical hypervisor host.
// Source: db/migrations/002_hosts.up.sql.
//
// VM-P2E Slice 1: Generation and DrainReason columns added via migration.
// VM-P2E Slice 2: Generation and DrainReason now scanned in all read paths.
type HostRecord struct {
	ID               string
	AvailabilityZone string
	Status           string
	Generation       int64   // optimistic concurrency counter; incremented on every status CAS
	DrainReason      *string // operator-supplied drain reason; nil if not set
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
func (r *Repo) GetAvailableHosts(ctx context.Context) ([]*HostRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, availability_zone, status,
		       generation, drain_reason,
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
			&h.Generation, &h.DrainReason,
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
func (r *Repo) GetHostByID(ctx context.Context, hostID string) (*HostRecord, error) {
	h := &HostRecord{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, availability_zone, status,
		       generation, drain_reason,
		       total_cpu, total_memory_mb, total_disk_gb,
		       used_cpu, used_memory_mb, used_disk_gb,
		       agent_version, last_heartbeat_at, registered_at, updated_at
		FROM hosts WHERE id = $1
	`, hostID).Scan(
		&h.ID, &h.AvailabilityZone, &h.Status,
		&h.Generation, &h.DrainReason,
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
