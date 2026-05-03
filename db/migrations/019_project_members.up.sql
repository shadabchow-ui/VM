CREATE TABLE IF NOT EXISTS project_members (
    project_id   UUID NOT NULL REFERENCES projects(id),
    principal_id UUID NOT NULL,
    role         VARCHAR(64) NOT NULL DEFAULT 'roles/viewer',
    added_by     UUID NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, principal_id)
);

CREATE INDEX IF NOT EXISTS idx_project_members_project
    ON project_members (project_id);
