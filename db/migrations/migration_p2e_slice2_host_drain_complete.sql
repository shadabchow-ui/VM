-- Migration: VM-P2E Slice 2 — drain completion + stopped-instance reassociation hardening
--
-- Schema changes: NONE.
--
-- All columns required for Slice 2 (generation, drain_reason, host_id on instances,
-- status on instances) were added by Slice 1 or pre-exist in the baseline schema.
--
-- Slice 2 is a pure code-layer change:
--   - MarkHostDrained: new Repo method (draining→drained CAS gated on active count)
--   - DetachStoppedInstancesFromHost: now also sets updated_at
--   - GetHostByID / GetAvailableHosts: now scan generation + drain_reason
--   - New endpoint: POST /internal/v1/hosts/{id}/drain-complete
--   - DrainHost inventory method: now correctly forwards fromGeneration from request
--
-- No ALTER TABLE or CREATE INDEX is needed for this slice.
--
-- Index added for reference (already in Slice 1 migration; listed here for completeness):
--   idx_hosts_status_heartbeat ON hosts (status, last_heartbeat_at DESC) WHERE status='ready'
--
-- Forward-compatible notes for Slice 3+ (DO NOT apply here):
--   - A future idx_hosts_status index (without heartbeat filter) may be useful
--     for fast drain/drained status queries as fleet size grows.
--   - Host fencing will add a 'fenced' status — no schema change, just new state string.
--   - Degraded/unhealthy will add reason_code column in a later migration.
--
-- Apply:
--   psql $DATABASE_URL -f migrations/YYYYMMDD_p2e_slice2_host_drain_complete.sql
--
-- This file is a no-op marker migration for operational tracking.
-- Running it is safe and idempotent.

-- No-op: Slice 2 has no schema changes.
-- The following SELECT verifies the expected columns are present.
DO $$
BEGIN
  -- Verify generation column (added in Slice 1)
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'generation'
  ) THEN
    RAISE EXCEPTION 'hosts.generation column missing — run Slice 1 migration first';
  END IF;

  -- Verify drain_reason column (added in Slice 1)
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'drain_reason'
  ) THEN
    RAISE EXCEPTION 'hosts.drain_reason column missing — run Slice 1 migration first';
  END IF;

  -- Verify instances.host_id column (pre-existing)
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'instances' AND column_name = 'host_id'
  ) THEN
    RAISE EXCEPTION 'instances.host_id column missing — check baseline migration';
  END IF;

  RAISE NOTICE 'VM-P2E Slice 2 schema pre-conditions verified OK';
END $$;
