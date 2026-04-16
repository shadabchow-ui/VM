-- Migration: VM-P2E Slice 6 — recovery automation tied to host lifecycle
--
-- Prerequisites: Slices 1–5 migrations must be applied first.
--
-- Schema additions:
--   host_recovery_log   — persisted log of recovery decisions and outcomes.
--                         One row per recovery attempt per host.
--
-- Design decisions:
--   - Recovery decisions are logged explicitly so operators and internal tooling
--     can inspect what automation did and why (fence-gated or not).
--   - verdict is the key observability field:
--       "skipped_fence_required"  — automation saw fence_required=TRUE and did nothing.
--       "skipped_not_eligible"    — host status is not recoverable (e.g. retired).
--       "reactivated"             — drained host transitioned back to ready.
--       "drain_initiated"         — degraded/unhealthy host transition to draining
--                                   for drain-then-recover path.
--       "cas_failed"              — CAS generation mismatch; nothing written.
--       "error"                   — unexpected DB error during attempt.
--   - actor distinguishes automated recovery from operator-initiated calls.
--   - host_status_at_attempt and host_generation_at_attempt are the observed
--     values at the time the decision was made; snapshot for audit purposes.
--   - No UPDATE to this table — rows are append-only audit records.
--   - TTL/cleanup of old rows is out of scope for Phase 1/P2E.
--
-- No changes to the hosts or maintenance_campaigns tables are required.
-- Slice 6 reads fence_required, status, and generation from the existing
-- hosts table via GetHostByID / GetRecoveryEligibleHosts (new query).
--
-- Apply:
--   psql $DATABASE_URL -f migrations/20260416_p2e_slice6_recovery_automation.sql

-- ── Table ─────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS host_recovery_log (
    id                          TEXT        PRIMARY KEY,
    host_id                     TEXT        NOT NULL,
    -- verdict: what the recovery automation decided.
    -- Values: skipped_fence_required, skipped_not_eligible, reactivated,
    --         drain_initiated, cas_failed, error
    verdict                     TEXT        NOT NULL,
    -- reason: human-readable explanation stored alongside the verdict.
    reason                      TEXT        NOT NULL DEFAULT '',
    -- host_status_at_attempt: the host's status at the moment of this attempt.
    -- Snapshotted for audit; may differ from current status.
    host_status_at_attempt      TEXT        NOT NULL DEFAULT '',
    -- host_generation_at_attempt: the host's generation at the time of attempt.
    -- Snapshotted for audit; the CAS may have failed if this was stale.
    host_generation_at_attempt  BIGINT      NOT NULL DEFAULT 0,
    -- fence_required_at_attempt: the fence_required flag value at the time of attempt.
    -- FALSE means recovery was not blocked by fencing. TRUE means it was skipped.
    fence_required_at_attempt   BOOLEAN     NOT NULL DEFAULT FALSE,
    -- actor: identifies what initiated the recovery attempt.
    -- "operator" for direct API calls; "recovery_loop" for automated periodic runs.
    actor                       TEXT        NOT NULL DEFAULT 'operator',
    -- campaign_id: optional — set when the recovery attempt was triggered by a
    -- campaign's failed_host_ids list. NULL for standalone host recovery calls.
    campaign_id                 TEXT        REFERENCES maintenance_campaigns(id) ON DELETE SET NULL,
    attempted_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Indexes ───────────────────────────────────────────────────────────────────

-- Fast lookup by host_id for per-host recovery history.
CREATE INDEX IF NOT EXISTS idx_host_recovery_log_host_id
    ON host_recovery_log (host_id, attempted_at DESC);

-- Fast scan for recent campaign-scoped recovery attempts.
CREATE INDEX IF NOT EXISTS idx_host_recovery_log_campaign_id
    ON host_recovery_log (campaign_id, attempted_at DESC)
    WHERE campaign_id IS NOT NULL;

-- Fast scan for recent automation-initiated recovery events (monitoring).
CREATE INDEX IF NOT EXISTS idx_host_recovery_log_actor_at
    ON host_recovery_log (actor, attempted_at DESC);

-- ── Verification ──────────────────────────────────────────────────────────────

DO $$
BEGIN
  -- Verify Slice 5 prerequisites
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_name = 'maintenance_campaigns'
  ) THEN
    RAISE EXCEPTION 'maintenance_campaigns table missing — run Slice 5 migration first';
  END IF;

  -- Verify Slice 3 fencing prerequisite
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'fence_required'
  ) THEN
    RAISE EXCEPTION 'hosts.fence_required column missing — run Slice 3 migration first';
  END IF;

  -- Verify this migration
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_name = 'host_recovery_log'
  ) THEN
    RAISE EXCEPTION 'host_recovery_log table missing — this migration did not apply correctly';
  END IF;

  RAISE NOTICE 'VM-P2E Slice 6 schema verified OK';
END $$;
