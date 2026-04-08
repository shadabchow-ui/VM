package db

// db.go — PostgreSQL access layer for the compute platform.
//
// Design: one Repo struct, one Pool interface, all repo methods on Repo.
// The Pool interface matches *pgxpool.Pool exactly so the real pool satisfies it
// without any adapter. Tests can inject a fake Pool.
//
// Source: IMPLEMENTATION_PLAN_V1 §A1 (PostgreSQL as single source of truth, pgx/v5).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ── Pool interface ────────────────────────────────────────────────────────────
//
// Matches *pgxpool.Pool. The real service passes pgxpool.New(...) directly.
// Accepts variadic ...any for argument lists, matching pgx/v5 signatures.

// CommandTag is satisfied by pgconn.CommandTag.
type CommandTag interface {
	RowsAffected() int64
}

// Rows is satisfied by pgx.Rows.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// Row is satisfied by pgx.Row.
type Row interface {
	Scan(dest ...any) error
}

// Pool is the interface Repo uses to talk to PostgreSQL.
// *pgxpool.Pool satisfies this interface directly.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Close()
}

// ── Repo ─────────────────────────────────────────────────────────────────────

// Repo is the single database accessor. Construct once at startup; safe for concurrent use.
// In services: pass in *pgxpool.Pool (satisfies Pool).
// In tests: pass in a fake that implements Pool.
type Repo struct {
	pool Pool
}

// New constructs a Repo from any Pool implementation.
// In production: db.New(pgxpoolInstance).
func New(pool Pool) *Repo {
	return &Repo{pool: pool}
}

// DatabaseURL returns DATABASE_URL from env or panics with a useful message.
func DatabaseURL() string {
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		panic("DATABASE_URL is not set")
	}
	return u
}

// Ping verifies the connection. Call once at service startup.
func (r *Repo) Ping(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, "SELECT 1")
	if err != nil {
		return fmt.Errorf("db ping: %w", err)
	}
	return nil
}

// ── Domain errors ─────────────────────────────────────────────────────────────

// ErrHostNotFound is returned when a host_id does not exist in the hosts table.
var ErrHostNotFound = errors.New("host not found")

// ErrBootstrapTokenInvalid is returned when a token is missing, expired, or already consumed.
var ErrBootstrapTokenInvalid = errors.New("bootstrap token invalid, expired, or already used")

// ── HostRecord ────────────────────────────────────────────────────────────────

// HostRecord is the DB row representation of a physical hypervisor host.
// Source: db/migrations/002_hosts.up.sql.
type HostRecord struct {
	ID               string
	AvailabilityZone string
	Status           string
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

// CanFit reports whether the host has enough free resources for the given shape.
// Used by the scheduler's SelectHost. Source: IMPLEMENTATION_PLAN_V1 §C3.
func (h *HostRecord) CanFit(cpuCores, memoryMB, diskGB int) bool {
	return h.Status == "ready" &&
		(h.TotalCPU-h.UsedCPU) >= cpuCores &&
		(h.TotalMemoryMB-h.UsedMemoryMB) >= memoryMB &&
		(h.TotalDiskGB-h.UsedDiskGB) >= diskGB
}

// ── Host repo methods ─────────────────────────────────────────────────────────

// UpsertHost registers a new host or refreshes its declared capacity on re-registration.
// Sets status = 'ready'. Idempotent: safe to call on every agent restart.
// Source: 05-02-host-runtime-worker-design.md §Bootstrap step 8,
//         IMPLEMENTATION_PLAN_V1 §B1 (Host Agent v1: startup registration).
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

// UpdateHeartbeat applies the Host Agent's periodic resource utilization report.
// Returns ErrHostNotFound if the host_id is unregistered — agent must re-register.
// Source: RUNTIMESERVICE_GRPC_V1 §8 (30s heartbeat interval).
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

// GetAvailableHosts returns all ready hosts that sent a heartbeat within the last 90 seconds,
// ordered by free CPU descending. This is the primary input for the scheduler's SelectHost.
//
// 90s window = 3 × 30s heartbeat interval — tolerates one missed heartbeat.
// Source: RUNTIMESERVICE_GRPC_V1 §8, IMPLEMENTATION_PLAN_V1 §C3.
func (r *Repo) GetAvailableHosts(ctx context.Context) ([]*HostRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, availability_zone, status,
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
func (r *Repo) GetHostByID(ctx context.Context, hostID string) (*HostRecord, error) {
	h := &HostRecord{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, availability_zone, status,
		       total_cpu, total_memory_mb, total_disk_gb,
		       used_cpu, used_memory_mb, used_disk_gb,
		       agent_version, last_heartbeat_at, registered_at, updated_at
		FROM hosts WHERE id = $1
	`, hostID).Scan(
		&h.ID, &h.AvailabilityZone, &h.Status,
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

// ConsumeBootstrapToken atomically validates and marks a token used.
// tokenHash is SHA-256(raw_token) hex-encoded — the raw token is never stored.
// Returns the host_id the token was issued for, or ErrBootstrapTokenInvalid.
// Source: AUTH_OWNERSHIP_MODEL_V1 §6 "Token invalidated after first use".
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

// InsertBootstrapToken writes a new token for a host being provisioned.
// tokenHash must be SHA-256(raw_token) hex-encoded.
// ON CONFLICT replaces an unused token for the same host (re-provisioning).
// Source: AUTH_OWNERSHIP_MODEL_V1 §6.
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
