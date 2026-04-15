-- VM-P2D Slice 2: project_members table.
--
-- Source: P2_PROJECT_RBAC_MODEL.md §3.2 (membership schema),
--         §3.3 (membership invariants MEM-I-1 to MEM-I-5),
--         AUTH_OWNERSHIP_MODEL_V1 §3 (Phase 1 compatibility — additive only).
--
-- Phase 1 compatibility: this migration adds a new table only.
-- No existing rows are altered. All Phase 1 account-owned resources remain
-- fully operational without any schema changes.
-- Source: P2_MIGRATION_COMPATIBILITY_RULES.

CREATE TABLE project_members (
    id                   UUID PRIMARY KEY,
    project_id           UUID NOT NULL REFERENCES projects(id),
    account_principal_id UUID NOT NULL REFERENCES principals(id),  -- must be ACCOUNT type
    role                 VARCHAR(20) NOT NULL,                      -- 'OWNER' | 'EDITOR' | 'VIEWER'
    invited_by           UUID REFERENCES principals(id),
    joined_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    removed_at           TIMESTAMPTZ
);

-- Enforces MEM-I-4: one active membership per (project, account).
CREATE UNIQUE INDEX idx_project_members_unique
    ON project_members (project_id, account_principal_id)
    WHERE removed_at IS NULL;

-- Fast lookup: "which projects is this account a member of?"
-- Used by ListInstancesVisible to build the project principal_id set.
CREATE INDEX idx_project_members_account
    ON project_members (account_principal_id)
    WHERE removed_at IS NULL;
