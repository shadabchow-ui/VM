-- 005_snapshots.up.sql — VM-P2B-S2: Snapshot schema.
--
-- Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.8 (snapshots table),
--         vm-15-02__skill__snapshot-clone-restore-retention-model.md.
--
-- Tables added:
--   1. snapshots               — first-class snapshot resource
--
-- Existing table changes:
--   2. volumes.source_snapshot_id — add FK now that snapshots table exists
--   3. jobs.snapshot_id           — new nullable FK for snapshot-scoped jobs
--
-- Design:
--   - owner_principal_id is VARCHAR(64) to match the existing volumes/instances pattern.
--   - source_volume_id and source_instance_id are nullable (source may be deleted post-snap).
--   - The jobs table gains snapshot_id alongside volume_id; exactly one subject column
--     is non-null per job row (enforced at application layer, same as volume_id).
--
-- Source: P2_MIGRATION_COMPATIBILITY_RULES.md §7 (no destructive changes to Phase 1 tables).

-- ── 1. snapshots ──────────────────────────────────────────────────────────────
-- Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.8.
CREATE TABLE IF NOT EXISTS snapshots (
    id                  VARCHAR(64)     NOT NULL PRIMARY KEY,   -- snap_ + KSUID
    owner_principal_id  VARCHAR(64)     NOT NULL,
    display_name        VARCHAR(63)     NOT NULL,
    region              VARCHAR(64)     NOT NULL,
    source_volume_id    VARCHAR(64)     REFERENCES volumes(id),    -- nullable; source may be deleted
    source_instance_id  VARCHAR(64)     REFERENCES instances(id),  -- nullable; set when snapping root disk
    size_gb             INTEGER         NOT NULL
                            CONSTRAINT snapshots_size_gb_positive CHECK (size_gb > 0),
    status              VARCHAR(20)     NOT NULL DEFAULT 'pending'
                            CONSTRAINT snapshots_status_check CHECK (
                                status IN ('pending', 'creating', 'available', 'error', 'deleting', 'deleted')
                            ),
    progress_percent    INTEGER         NOT NULL DEFAULT 0
                            CONSTRAINT snapshots_progress_check CHECK (progress_percent BETWEEN 0 AND 100),
    storage_path        VARCHAR(1024),              -- set after creation completes
    storage_pool_id     VARCHAR(64),
    encrypted           BOOLEAN         NOT NULL DEFAULT FALSE,
    version             INTEGER         NOT NULL DEFAULT 0,
    locked_by           VARCHAR(64),               -- job_id holding exclusive mutation lock
    created_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    completed_at        TIMESTAMPTZ,               -- set when status → available
    updated_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ
);

-- Owner listing, newest first.
CREATE INDEX IF NOT EXISTS idx_snapshots_owner_created
    ON snapshots (owner_principal_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- Fast status-based scans.
CREATE INDEX IF NOT EXISTS idx_snapshots_status
    ON snapshots (status)
    WHERE deleted_at IS NULL;

-- Source volume lookup (e.g. "which snapshots exist for this volume?").
CREATE INDEX IF NOT EXISTS idx_snapshots_source_volume
    ON snapshots (source_volume_id)
    WHERE source_volume_id IS NOT NULL AND deleted_at IS NULL;

-- ── 2. volumes.source_snapshot_id — add FK now that snapshots table exists ───
-- The column was added in 004 as VARCHAR(64) without a FK (table didn't exist yet).
-- Add the constraint now.
-- Source: P2_VOLUME_MODEL.md §6 (source_snapshot_id FK deferred until snapshots table).
ALTER TABLE volumes
    ADD CONSTRAINT fk_volumes_source_snapshot
        FOREIGN KEY (source_snapshot_id) REFERENCES snapshots(id)
        NOT VALID;   -- NOT VALID: skip checking existing NULLs; safe because all existing rows are NULL.

-- ── 3. jobs.snapshot_id ───────────────────────────────────────────────────────
-- Snapshot jobs have no instance or volume context — they carry snapshot_id.
-- Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (async job dispatch for SNAPSHOT_CREATE / SNAPSHOT_DELETE).
ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS snapshot_id VARCHAR(64) REFERENCES snapshots(id);

-- Fast snapshot-job lookup.
CREATE INDEX IF NOT EXISTS idx_jobs_snapshot_id_status
    ON jobs (snapshot_id, status)
    WHERE snapshot_id IS NOT NULL;
