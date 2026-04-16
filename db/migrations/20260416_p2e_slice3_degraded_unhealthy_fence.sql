-- Migration: VM-P2E Slice 3 — degraded / unhealthy host lifecycle states + fencing groundwork
--
-- Prerequisites: Slice 1 and Slice 2 migrations must be applied first.
--
-- Schema changes:
--   hosts.reason_code   TEXT     — machine-readable reason for the current status.
--                                  Populated on transitions to degraded/unhealthy.
--                                  Cleared on transition back to ready.
--                                  NULL for hosts in ready/draining/drained states.
--   hosts.fence_required BOOLEAN NOT NULL DEFAULT FALSE
--                                — set TRUE when a host enters an ambiguous failure
--                                  state requiring fencing before recovery automation
--                                  may proceed. Set FALSE when host returns to ready
--                                  or is explicitly cleared by an operator.
--
-- New index:
--   idx_hosts_fence_required — fast scan for any host with fence_required=TRUE.
--   Used by operator tooling and future fencing controller. Non-blocking in prod
--   (CREATE INDEX CONCURRENTLY).
--
-- State machine after this slice:
--   ready       — schedulable, healthy
--   draining    — unschedulable, draining workloads (Slice 1)
--   drained     — unschedulable, all workloads gone (Slice 2)
--   degraded    — NEW: unschedulable, health signals degraded but recoverable
--   unhealthy   — NEW: unschedulable, health signals failed; fence may be required
--   fenced      — future Slice 4+: host isolated, recovery may proceed
--   retired     — future Slice 4+: permanently removed from service
--
-- Reason codes (reason_code values) defined in code (see host_transition_rules.go):
--   AGENT_UNRESPONSIVE   — heartbeat missed beyond degraded threshold
--   AGENT_FAILED         — agent health probe failed explicitly
--   STORAGE_ERROR        — local storage I/O error or read-only filesystem
--   HYPERVISOR_FAILED    — hypervisor daemon (firecracker) unresponsive
--   NETWORK_UNREACHABLE  — data-plane network connectivity lost
--   OPERATOR_DEGRADED    — operator manually marked host degraded
--   OPERATOR_UNHEALTHY   — operator manually marked host unhealthy
--
-- fence_required semantics:
--   FALSE (default): transition to unhealthy is ambiguous but recovery is not
--                    yet blocked by a fence requirement.
--   TRUE:  the host failure is ambiguous enough that recovery automation must
--          NOT proceed until a fencing controller explicitly clears this flag.
--          Which reason codes set fence_required=TRUE is defined in
--          MarkHostUnhealthy (reason_code IN AGENT_UNRESPONSIVE, HYPERVISOR_FAILED,
--          NETWORK_UNREACHABLE — ambiguous split-brain risk).
--
-- Transition safety:
--   - Both columns are additive: DEFAULT FALSE / NULL means no migration of
--     existing rows is required. All existing hosts are unaffected.
--   - The new index is non-unique and non-blocking (CREATE INDEX CONCURRENTLY
--     in production).
--   - No existing column is modified or dropped.
--
-- Apply:
--   psql $DATABASE_URL -f migrations/YYYYMMDD_p2e_slice3_degraded_unhealthy_fence.sql
--
-- Forward-compatible notes for Slice 4+:
--   - 'fenced' and 'retired' status strings are reserved; no schema change needed
--     (they are just new status values in the existing TEXT column).
--   - The fencing controller in Slice 4 will scan fence_required=TRUE and set
--     hosts.status='fenced' after confirming isolation; it will also set
--     fence_required=FALSE at that point.
--   - A replacement/retirement workflow in Slice 4 will set status='retired'.

-- ── Column additions ──────────────────────────────────────────────────────────

ALTER TABLE hosts
  ADD COLUMN IF NOT EXISTS reason_code    TEXT,
  ADD COLUMN IF NOT EXISTS fence_required BOOLEAN NOT NULL DEFAULT FALSE;

-- ── Index: fence_required fast scan ──────────────────────────────────────────
-- Allows operators and future fencing controllers to efficiently find all hosts
-- that require fencing before recovery.
-- In production: CREATE INDEX CONCURRENTLY to avoid locking writes.

CREATE INDEX IF NOT EXISTS idx_hosts_fence_required
    ON hosts (fence_required, updated_at DESC)
    WHERE fence_required = TRUE;

-- ── Verification ──────────────────────────────────────────────────────────────

DO $$
BEGIN
  -- Verify Slice 1 prerequisite
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'generation'
  ) THEN
    RAISE EXCEPTION 'hosts.generation column missing — run Slice 1 migration first';
  END IF;

  -- Verify Slice 1 prerequisite
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'drain_reason'
  ) THEN
    RAISE EXCEPTION 'hosts.drain_reason column missing — run Slice 1 migration first';
  END IF;

  -- Verify this migration's columns
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'reason_code'
  ) THEN
    RAISE EXCEPTION 'hosts.reason_code column missing — this migration did not apply correctly';
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'hosts' AND column_name = 'fence_required'
  ) THEN
    RAISE EXCEPTION 'hosts.fence_required column missing — this migration did not apply correctly';
  END IF;

  RAISE NOTICE 'VM-P2E Slice 3 schema verified OK';
END $$;
