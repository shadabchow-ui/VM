-- Migration: VM-P2E Slice 5 — maintenance orchestration foundation
--
-- Prerequisites: Slices 1–4 migrations must be applied first.
--
-- Schema additions:
--   maintenance_campaigns   — persists one record per maintenance campaign.
--                             A campaign is an operator-defined batch of hosts
--                             that should be drained/retired in a controlled
--                             sequence with blast-radius limits enforced.
--
-- Design decisions:
--   - Campaigns are a separate table rather than a column on hosts so that a
--     single campaign can reference many hosts without denormalizing host rows.
--   - host_ids is stored as a TEXT[] (PostgreSQL native array) so the full
--     target set is visible in a single row; no join table is needed for the
--     Slice 5 scope.
--   - max_parallel is stored here (not derived on query) so it is immutable
--     after creation — blast-radius intent is locked at creation time.
--   - status uses TEXT (not ENUM) consistent with hosts.status throughout the
--     codebase. Valid values: pending, running, paused, completed, cancelled.
--   - completed_host_ids and failed_host_ids track progress without a
--     separate join table. Arrays stay small in practice (campaign batches are
--     bounded by max_parallel and blast_radius_pct).
--   - campaign_reason is a human-readable label for observability (e.g.
--     "kernel-4.19 patch", "DC migration batch 1").
--   - created_at / updated_at follow the pattern on the hosts table.
--
-- Forward-compatibility notes for Slice 6:
--   - Slice 6 recovery automation can observe campaign status and the
--     failed_host_ids list via GetCampaignByID / ListCampaigns.
--   - No schema change will be needed in Slice 6 to read these fields.
--   - failed_host_ids is the seam: a recovery actor checks this list and
--     decides whether to attempt host-level recovery actions.
--
-- Apply:
--   psql $DATABASE_URL -f migrations/20260416_p2e_slice5_maintenance_campaigns.sql

-- ── Table ─────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS maintenance_campaigns (
    id                  TEXT        PRIMARY KEY,
    campaign_reason     TEXT        NOT NULL,
    -- target_host_ids: full list of hosts submitted to this campaign.
    -- Immutable after creation.
    target_host_ids     TEXT[]      NOT NULL DEFAULT '{}',
    -- completed_host_ids: hosts whose drain/retire action succeeded.
    -- Updated by AdvanceCampaign as individual hosts complete.
    completed_host_ids  TEXT[]      NOT NULL DEFAULT '{}',
    -- failed_host_ids: hosts whose action failed or were blocked unexpectedly.
    -- Slice 6 recovery seam: recovery automation inspects this list.
    failed_host_ids     TEXT[]      NOT NULL DEFAULT '{}',
    -- max_parallel: maximum number of hosts that may be acted on concurrently.
    -- This is the blast-radius limit. Immutable after creation.
    -- Valid range: 1..len(target_host_ids), capped at MaxCampaignParallel.
    max_parallel        INT         NOT NULL DEFAULT 1,
    -- status: campaign lifecycle state.
    -- pending   — created, not yet started
    -- running   — at least one host action is in flight
    -- paused    — operator halted; no new hosts will be advanced
    -- completed — all target hosts have been actioned (success or failure)
    -- cancelled — operator cancelled; in-flight hosts still drain naturally
    status              TEXT        NOT NULL DEFAULT 'pending',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Indexes ───────────────────────────────────────────────────────────────────

-- Fast scan for active (running/pending) campaigns; operator and monitoring queries.
CREATE INDEX IF NOT EXISTS idx_maintenance_campaigns_status
    ON maintenance_campaigns (status)
    WHERE status IN ('pending', 'running', 'paused');

-- ── Verification ──────────────────────────────────────────────────────────────

DO $$
BEGIN
  -- Verify Slice 4 prerequisites
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'retired_at'
  ) THEN
    RAISE EXCEPTION 'hosts.retired_at missing — run Slice 4 migration first';
  END IF;

  -- Verify this migration
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_name = 'maintenance_campaigns'
  ) THEN
    RAISE EXCEPTION 'maintenance_campaigns table missing — this migration did not apply correctly';
  END IF;

  RAISE NOTICE 'VM-P2E Slice 5 schema verified OK';
END $$;
