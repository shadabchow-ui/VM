-- migration: 0016_iam_metering.sql
-- Phase 16B: Service Account, Role Binding, Usage Record, Reconciliation Hold,
-- and Budget Policy tables.
--
-- Source:
--   vm-16-01__blueprint__ §mvp, §core_contracts
--   vm-16-02__blueprint__ §mvp, §core_contracts
--   iam_repo.go, metering_repo.go (authoritative API surface)
--   AUTH_OWNERSHIP_MODEL_V1 §3 (project-scoped ownership)
--
-- Scope: additive only. Does not modify any existing table.
-- Idempotent: uses CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT EXISTS.

-- ── Service Accounts ──────────────────────────────────────────────────────────
--
-- service_accounts are scoped to a project.
-- Soft-delete: deleted_at IS NULL means active.
-- status: 'active' | 'disabled'
-- unique (project_id, name): prevents duplicate names within a project.

CREATE TABLE IF NOT EXISTS service_accounts (
    id            VARCHAR(64)  PRIMARY KEY,
    project_id    VARCHAR(64)  NOT NULL,
    name          VARCHAR(128) NOT NULL,
    display_name  VARCHAR(256) NOT NULL DEFAULT '',
    description   TEXT,
    status        VARCHAR(32)  NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    created_by    VARCHAR(64)  NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ,
    UNIQUE (project_id, name)
);

CREATE INDEX IF NOT EXISTS idx_service_accounts_project
    ON service_accounts (project_id)
    WHERE deleted_at IS NULL;

-- ── Role Bindings ─────────────────────────────────────────────────────────────
--
-- role_bindings store (project_id, principal_id, role, resource_type, resource_id)
-- tuples. ON CONFLICT DO NOTHING ensures idempotent grant.
-- resource_type: 'project' | 'service_account'
-- resource_id: the ID of the resource (project ID or SA ID)
-- role: 'roles/owner' | 'roles/compute.viewer'

CREATE TABLE IF NOT EXISTS role_bindings (
    id            VARCHAR(64)  PRIMARY KEY,
    project_id    VARCHAR(64)  NOT NULL,
    principal_id  VARCHAR(64)  NOT NULL,
    role          VARCHAR(128) NOT NULL,
    resource_type VARCHAR(64)  NOT NULL,
    resource_id   VARCHAR(64)  NOT NULL,
    granted_by    VARCHAR(64)  NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, principal_id, role, resource_type, resource_id)
);

CREATE INDEX IF NOT EXISTS idx_role_bindings_project
    ON role_bindings (project_id);

CREATE INDEX IF NOT EXISTS idx_role_bindings_principal
    ON role_bindings (project_id, principal_id);

-- ── Usage Records ─────────────────────────────────────────────────────────────
--
-- usage_records is the append-only event log for metering.
-- event_id UNIQUE enforces exactly-once ingestion (ON CONFLICT DO NOTHING in repo).
-- record_type: 'USAGE_START' | 'USAGE_END' | 'RECONCILED'
-- ended_at IS NULL → record is open (instance still running).
-- duration_seconds is computed on CloseUsageRecord.

CREATE TABLE IF NOT EXISTS usage_records (
    id               VARCHAR(64)  PRIMARY KEY,
    instance_id      VARCHAR(64)  NOT NULL,
    scope_id         VARCHAR(64)  NOT NULL,
    project_id       VARCHAR(64),
    record_type      VARCHAR(32)  NOT NULL
        CHECK (record_type IN ('USAGE_START', 'USAGE_END', 'RECONCILED')),
    instance_type_id VARCHAR(64)  NOT NULL DEFAULT '',
    started_at       TIMESTAMPTZ  NOT NULL,
    ended_at         TIMESTAMPTZ,
    duration_seconds BIGINT,
    event_id         VARCHAR(256) NOT NULL UNIQUE,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (ended_at IS NULL OR ended_at > started_at)
);

CREATE INDEX IF NOT EXISTS idx_usage_records_instance
    ON usage_records (instance_id, started_at DESC);

CREATE INDEX IF NOT EXISTS idx_usage_records_scope
    ON usage_records (scope_id, started_at DESC);

CREATE INDEX IF NOT EXISTS idx_usage_records_open
    ON usage_records (instance_id)
    WHERE ended_at IS NULL;

-- ── Reconciliation Holds ──────────────────────────────────────────────────────
--
-- reconciliation_holds must be written atomically before synthesizing any
-- missing usage event to prevent double-billing.
-- UNIQUE (instance_id, window_start, window_end) prevents duplicate holds.
-- status: 'pending' | 'applied' | 'released'
--
-- Source: vm-16-02__blueprint__ §core_contracts "Reservation-Based Exactly-Once Guarantee".

CREATE TABLE IF NOT EXISTS reconciliation_holds (
    id           VARCHAR(64) PRIMARY KEY,
    instance_id  VARCHAR(64)  NOT NULL,
    scope_id     VARCHAR(64)  NOT NULL,
    window_start TIMESTAMPTZ  NOT NULL,
    window_end   TIMESTAMPTZ  NOT NULL,
    status       VARCHAR(32)  NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'applied', 'released')),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (instance_id, window_start, window_end),
    CHECK (window_end > window_start)
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_holds_instance
    ON reconciliation_holds (instance_id, window_start);

CREATE INDEX IF NOT EXISTS idx_reconciliation_holds_pending
    ON reconciliation_holds (status)
    WHERE status = 'pending';

-- ── Budget Policies ───────────────────────────────────────────────────────────
--
-- budget_policies are scoped to a principal scope (scope_id).
-- limit_cents and accrued_cents are stored in integer cents to avoid float math.
-- enforcement_action: 'notify' (MVP) | 'block_create'
--   'block_create' is validated but quota lock enforcement is deferred.
--   Source: vm-16-02__blueprint__ §mvp "deferred: automated quota locks".
-- status: 'active' | 'disabled'

CREATE TABLE IF NOT EXISTS budget_policies (
    id                 VARCHAR(64) PRIMARY KEY,
    scope_id           VARCHAR(64)  NOT NULL,
    project_id         VARCHAR(64),
    limit_cents        BIGINT       NOT NULL CHECK (limit_cents > 0),
    accrued_cents      BIGINT       NOT NULL DEFAULT 0,
    period_start       TIMESTAMPTZ  NOT NULL,
    period_end         TIMESTAMPTZ  NOT NULL,
    enforcement_action VARCHAR(32)  NOT NULL DEFAULT 'notify'
        CHECK (enforcement_action IN ('notify', 'block_create')),
    notification_email VARCHAR(256),
    status             VARCHAR(32)  NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    created_by         VARCHAR(64)  NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (period_end > period_start)
);

CREATE INDEX IF NOT EXISTS idx_budget_policies_scope_active
    ON budget_policies (scope_id, status)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_budget_policies_exceeded
    ON budget_policies (scope_id)
    WHERE status             = 'active'
      AND enforcement_action = 'block_create'
      AND accrued_cents      >= limit_cents;
