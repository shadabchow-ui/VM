-- 017_phase16a_iam.up.sql
--
-- Phase 16A: Org/Project IAM, RBAC, and Service Account model persistence.
--
-- Adds:
--   service_accounts    — workload identity resources scoped to a project
--   iam_role_bindings   — policy bindings: (principal, role, resource) within a project
--
-- What is deferred to later migrations:
--   organizations, folders (hierarchy above project) — Phase 16B
--   iam_credentials / token_vending               — future IAM Credentials Service
--   materialized_path column on projects           — Phase 16B hierarchy seam
--
-- Design decisions:
--   service_accounts: soft-delete via deleted_at; status is 'active'|'disabled'|'deleted'.
--   iam_role_bindings: hard-delete; removals must propagate immediately per
--     "Policy Propagation is Fast and Consistent" contract.
--   Cross-account safety: all queries include project_id in WHERE clause so
--     a principal cannot read another project's resources by ID alone.
--
-- Source: vm-16-01__blueprint__ §components, §core_contracts,
--         AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).

-- ── service_accounts ─────────────────────────────────────────────────────────
--
-- Each service account is a workload identity resource belonging to exactly
-- one project.  status transitions: active → disabled → active (reversible);
-- active|disabled → deleted (soft-delete, irreversible within this migration).
-- UNIQUE (project_id, name) enforces namespace uniqueness within a project.
--
-- Source: vm-16-01__research__ §"Service Account Lifecycle and Credential Model".

CREATE TABLE service_accounts (
    id           TEXT        NOT NULL PRIMARY KEY,
    project_id   TEXT        NOT NULL REFERENCES projects (id),
    name         TEXT        NOT NULL,           -- slug, unique within project
    display_name TEXT        NOT NULL,
    description  TEXT,
    status       TEXT        NOT NULL DEFAULT 'active'
                             CHECK (status IN ('active', 'disabled', 'deleted')),
    created_by   TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at   TIMESTAMPTZ,
    UNIQUE (project_id, name)
);

CREATE INDEX idx_service_accounts_project ON service_accounts (project_id)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_service_accounts_created_by ON service_accounts (created_by)
    WHERE deleted_at IS NULL;

-- ── iam_role_bindings ─────────────────────────────────────────────────────────
--
-- Each row grants principal_id the named role on (resource_type, resource_id)
-- within the project.
--
-- Phase 16A role examples:
--   roles/owner             — full control within a project
--   roles/compute.viewer    — read-only on compute resources (future enforcement)
--   roles/iam.serviceAccountUser — actAs a service account (future enforcement)
--
-- resource_type values: 'project', 'instance', 'service_account'
-- resource_id: primary key of the target resource in the referenced table.
--
-- UNIQUE constraint enforces idempotent binding creation: calling
-- CreateRoleBinding twice for the same (project, principal, role, resource)
-- is a no-op via ON CONFLICT DO NOTHING.
--
-- Hard-delete: role binding rows are deleted outright — no soft-delete —
-- so revocations take effect immediately (within query latency).
--
-- Source: vm-16-01__blueprint__ §core_contracts
--         "Authorization is Hierarchical and Additive",
--         "Policy Propagation is Fast and Consistent".

CREATE TABLE iam_role_bindings (
    id            TEXT        NOT NULL PRIMARY KEY,
    project_id    TEXT        NOT NULL REFERENCES projects (id),
    principal_id  TEXT        NOT NULL,   -- user principal or service account principal_id
    role          TEXT        NOT NULL,   -- e.g. 'roles/owner', 'roles/compute.viewer'
    resource_type TEXT        NOT NULL,   -- 'project' | 'instance' | 'service_account'
    resource_id   TEXT        NOT NULL,   -- PK of the target resource
    granted_by    TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, principal_id, role, resource_type, resource_id)
);

CREATE INDEX idx_iam_role_bindings_project ON iam_role_bindings (project_id);

-- Lookup index: find all roles a principal holds in a project.
CREATE INDEX idx_iam_role_bindings_principal ON iam_role_bindings (project_id, principal_id);

-- Authorization check index: check(principal, role, resource_type, resource_id).
-- This is the hot path for CheckPrincipalHasRole (Phase 16A) and the future
-- IAM Policy Service check() endpoint (Phase 16B).
CREATE INDEX idx_iam_role_bindings_check ON iam_role_bindings
    (project_id, principal_id, role, resource_type, resource_id);
