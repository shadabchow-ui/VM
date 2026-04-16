-- 018_phase16a_metering.up.sql
--
-- Phase 16A: Usage metering, billing, and budget controls persistence.
--
-- Adds:
--   usage_records         — immutable log of billable consumption events
--   reconciliation_holds  — reservation slots to prevent double-billing
--   budget_policies       — per-scope spending caps with enforcement actions
--
-- Architecture rules baked into this schema:
--
--   1. usage_records is INSERT-only. Never UPDATE or DELETE a usage row.
--      Corrections are new rows with record_type='ADJUSTMENT'.
--      Source: vm-16-02__blueprint__ §core_contracts
--              "Event Sourced Single Source of Truth".
--
--   2. reconciliation_holds must be written BEFORE any synthetic usage event
--      is injected. The UNIQUE (instance_id, window_start, window_end) constraint
--      makes hold acquisition atomic and idempotent.
--      Source: vm-16-02__blueprint__ §core_contracts
--              "Reservation-Based Exactly-Once Guarantee".
--
--   3. budget_policies enforce spending via accrued_cents >= limit_cents checks.
--      Enforcement action 'block_create' blocks new resource creation only.
--      Running instances are NEVER terminated by the platform due to budget.
--      Source: vm-16-02__blueprint__ §core_contracts
--              "Non-Destructive Budget Enforcement".
--
-- What is deferred:
--   Pricing catalog / rating tables     — Phase 16B Rating & Pricing Subsystem
--   Invoice / billing_period tables     — Phase 16B batch pipeline
--   Real-time OLAP materialized views   — Phase 16B streaming path
--   Automated DLQ tables                — Phase 16B operational maturity
--
-- Source: vm-16-02__blueprint__ §components,
--         vm-16-02__research__ §"Metering & Ingestion Subsystem".

-- ── usage_records ─────────────────────────────────────────────────────────────
--
-- Immutable event log.  One row per billable event (start, end, reconciled, adjustment).
-- event_id is the metering idempotency key: ON CONFLICT (event_id) DO NOTHING
-- in InsertUsageRecord ensures at-most-once insertion for duplicate agent events.
--
-- scope_id = owner_principal_id at the time of recording.
--   Classic mode:  scope_id == user principal_id
--   Project mode:  scope_id == project.principal_id
-- This matches the existing quota scope anchor — no new scope model is introduced.
--
-- project_id is nullable: NULL for classic/no-project instances.
--
-- Source: vm-16-02__blueprint__ §components "Metering & Ingestion Subsystem",
--         instance_handlers.go §"VM-P2D Slice 4: Project scope resolution".

CREATE TABLE usage_records (
    id               TEXT        NOT NULL PRIMARY KEY,
    instance_id      TEXT        NOT NULL REFERENCES instances (id),
    scope_id         TEXT        NOT NULL,           -- owner_principal_id
    project_id       TEXT,                           -- NULL for classic mode
    record_type      TEXT        NOT NULL
                                 CHECK (record_type IN
                                        ('USAGE_START', 'USAGE_END', 'RECONCILED', 'ADJUSTMENT')),
    instance_type_id TEXT        NOT NULL,
    started_at       TIMESTAMPTZ NOT NULL,
    ended_at         TIMESTAMPTZ,                    -- NULL for open USAGE_START records
    duration_seconds BIGINT,                         -- NULL until closed
    event_id         TEXT        NOT NULL,           -- metering idempotency key
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (event_id)
);

-- Billing query index: get usage for a scope ordered by time.
CREATE INDEX idx_usage_records_scope ON usage_records (scope_id, started_at DESC);

-- Instance lookup: find open intervals for reconciliation.
CREATE INDEX idx_usage_records_instance ON usage_records (instance_id, record_type)
    WHERE ended_at IS NULL;

-- ── reconciliation_holds ──────────────────────────────────────────────────────
--
-- A hold reserves a (instance_id, window_start, window_end) slot before the
-- reconciliation service synthesizes a missing usage event.
-- The UNIQUE constraint makes hold acquisition atomic: only one reconciler
-- instance can win the race for a given window.
--
-- status: pending → applied (synthetic event written)
--         pending → released (original event arrived; no synthetic needed)
--
-- Source: vm-16-02__blueprint__ §core_contracts
--         "Reservation-Based Exactly-Once Guarantee".

CREATE TABLE reconciliation_holds (
    id           TEXT        NOT NULL PRIMARY KEY,
    instance_id  TEXT        NOT NULL REFERENCES instances (id),
    scope_id     TEXT        NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end   TIMESTAMPTZ NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'applied', 'released')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (instance_id, window_start, window_end)
);

CREATE INDEX idx_reconciliation_holds_instance ON reconciliation_holds (instance_id, status);

-- ── budget_policies ───────────────────────────────────────────────────────────
--
-- Per-scope (user or project) spending caps.
-- limit_cents = 0 means the policy is disabled (no cap).
-- accrued_cents is updated by IncrementBudgetAccrual on the streaming path
-- (best-effort approximation; authoritative figure is from the usage_records batch).
--
-- enforcement_action:
--   'notify'       — send notification only; CreateInstance is not blocked.
--   'block_create' — CheckBudgetAllowsCreate returns ErrBudgetExceeded when
--                    accrued_cents >= limit_cents. Running instances are safe.
--
-- status: 'active' | 'paused' | 'expired'
--   expired: period_end has passed; policy no longer evaluated.
--
-- Source: vm-16-02__blueprint__ §components "Budget & Quota Enforcement Subsystem",
--         §core_contracts "Non-Destructive Budget Enforcement".

CREATE TABLE budget_policies (
    id                  TEXT        NOT NULL PRIMARY KEY,
    scope_id            TEXT        NOT NULL,  -- owner_principal_id the policy applies to
    project_id          TEXT,                  -- NULL for user-level policies
    limit_cents         BIGINT      NOT NULL DEFAULT 0,
    accrued_cents       BIGINT      NOT NULL DEFAULT 0,
    period_start        TIMESTAMPTZ NOT NULL,
    period_end          TIMESTAMPTZ NOT NULL,
    enforcement_action  TEXT        NOT NULL DEFAULT 'notify'
                                    CHECK (enforcement_action IN ('notify', 'block_create')),
    notification_email  TEXT,
    status              TEXT        NOT NULL DEFAULT 'active'
                                    CHECK (status IN ('active', 'paused', 'expired')),
    created_by          TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (period_end > period_start),
    CHECK (limit_cents >= 0),
    CHECK (accrued_cents >= 0)
);

-- Admission check index: CheckBudgetAllowsCreate hot path.
CREATE INDEX idx_budget_policies_scope_active ON budget_policies (scope_id, status, period_end)
    WHERE status = 'active';
