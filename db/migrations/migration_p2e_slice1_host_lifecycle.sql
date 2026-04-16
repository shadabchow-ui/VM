-- Migration: VM-P2E Slice 1 — host lifecycle state foundation
--
-- Adds to the existing hosts table:
--   generation  INT NOT NULL DEFAULT 0  — optimistic concurrency for status CAS
--   drain_reason TEXT                   — operator-supplied reason for drain
--
-- Adds new schedulable-status index to accelerate ListReadyHosts (scheduler path).
--
-- Source: vm-13-03__blueprint__ §core_contracts "Host State Atomicity",
--         §implementation_decisions "optimistic concurrency control on host state".
--
-- Safety:
--   - generation column: DEFAULT 0 means all existing rows start at generation 0.
--     The CAS in UpdateHostStatus will succeed on first call with expectedGeneration=0.
--   - drain_reason: nullable, safe to add without default.
--   - The new index is non-unique and non-blocking (CREATE INDEX CONCURRENTLY in prod).
--   - No existing column is modified or dropped.
--
-- Apply:
--   psql $DATABASE_URL -f migrations/YYYYMMDD_p2e_slice1_host_lifecycle.sql

-- ── Column additions ──────────────────────────────────────────────────────────

ALTER TABLE hosts
  ADD COLUMN IF NOT EXISTS generation   INT  NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS drain_reason TEXT;

-- ── Index: scheduler admission fast path ──────────────────────────────────────
-- Accelerates GET /internal/v1/hosts (ListReadyHosts) which filters on status='ready'
-- and last_heartbeat_at > NOW() - INTERVAL '90 seconds'.
-- In production use CREATE INDEX CONCURRENTLY to avoid locking writes.

CREATE INDEX IF NOT EXISTS idx_hosts_status_heartbeat
    ON hosts (status, last_heartbeat_at DESC)
    WHERE status = 'ready';
