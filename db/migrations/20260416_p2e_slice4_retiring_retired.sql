-- Migration: VM-P2E Slice 4 — host retiring / retired lifecycle states
--
-- Prerequisites: Slice 1, Slice 2, Slice 3 migrations must be applied first.
--
-- Schema changes:
--   hosts.retired_at  TIMESTAMPTZ   — wall-clock timestamp when the host was
--                                     transitioned to 'retired'. NULL until then.
--                                     Persisted for audit trail and replacement
--                                     capacity reporting by Slice 5+.
--
-- New status values (no schema change — stored in hosts.status TEXT):
--   retiring  — operator has initiated retirement; host is unschedulable and
--               being prepared for permanent removal. Workloads must already be
--               gone (transition only allowed from 'drained' or 'fenced').
--   retired   — host is permanently removed from service. Terminal state.
--               scheduler excludes it; no new workloads may be placed.
--
-- New reason code (see db.go ReasonXxx constants):
--   OPERATOR_RETIRED — operator explicitly retired the host. Stored in
--                      reason_code on the retiring/retired transition.
--
-- New index:
--   idx_hosts_retired — fast scan for retired hosts ordered by retired_at.
--   Used by replacement-seam query GetRetiredHosts (Slice 5+ capacity planning).
--
-- State machine after this slice:
--   ready       — schedulable, healthy
--   draining    — unschedulable, draining workloads (Slice 1)
--   drained     — unschedulable, all workloads gone (Slice 2)
--   degraded    — unschedulable, health signals degraded (Slice 3)
--   unhealthy   — unschedulable, health signals failed (Slice 3)
--   fenced      — future: host isolated after ambiguous failure (Slice 5+)
--   retiring    — NEW: unschedulable, operator-initiated retirement in progress
--   retired     — NEW: terminal; permanently removed from scheduler pool
--
-- Allowed retirement transitions (enforced in legalTransitions):
--   drained  → retiring   (normal path: drain first, then retire)
--   fenced   → retiring   (emergency path: fenced host being decommissioned)
--   retiring → retired    (retirement completes after operator confirms)
--   drained  → retired    (direct admin-only shortcut; kept explicit and narrow)
--
-- Transition safety:
--   - retired_at is additive (DEFAULT NULL). No existing rows affected.
--   - No enum column change — status is TEXT throughout the codebase.
--   - The new index is non-unique. Use CONCURRENTLY in production.
--   - No existing column is modified or dropped.
--
-- Forward-compatible notes for Slice 5+:
--   - GetRetiredHosts query returns hosts ordered by retired_at DESC so a
--     Slice 5 replacement orchestrator can process oldest-retired hosts first.
--   - The fenced → retiring path is wired now so a Slice 5 fencing controller
--     can retire fenced hosts without a schema or transition-table change.
--   - A future capacity manager will query retired hosts to backfill capacity.
--
-- Apply:
--   psql $DATABASE_URL -f migrations/20260416_p2e_slice4_retiring_retired.sql

-- ── Column additions ──────────────────────────────────────────────────────────

ALTER TABLE hosts
  ADD COLUMN IF NOT EXISTS retired_at TIMESTAMPTZ;

-- ── Index: retired host scan ──────────────────────────────────────────────────
-- Allows Slice 5+ replacement workflows to efficiently enumerate retired hosts
-- and calculate capacity deficit. Partial index keeps it small.
-- In production: CREATE INDEX CONCURRENTLY to avoid locking writes.

CREATE INDEX IF NOT EXISTS idx_hosts_retired
    ON hosts (retired_at DESC)
    WHERE status = 'retired';

-- ── Verification ──────────────────────────────────────────────────────────────

DO $$
BEGIN
  -- Verify Slice 1 prerequisites
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'generation'
  ) THEN
    RAISE EXCEPTION 'hosts.generation missing — run Slice 1 migration first';
  END IF;

  -- Verify Slice 3 prerequisites
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'fence_required'
  ) THEN
    RAISE EXCEPTION 'hosts.fence_required missing — run Slice 3 migration first';
  END IF;

  -- Verify this migration's column
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'retired_at'
  ) THEN
    RAISE EXCEPTION 'hosts.retired_at missing — this migration did not apply correctly';
  END IF;

  RAISE NOTICE 'VM-P2E Slice 4 schema verified OK';
END $$;
