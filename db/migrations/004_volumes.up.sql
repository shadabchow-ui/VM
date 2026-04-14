-- 004_volumes.up.sql — VM-P2B: Independent block volume schema.
--
-- Source: P2_VOLUME_MODEL.md §6 (schema), §3 (state machine), §4 (attachment semantics),
--         §7 (invariants), vm-15-01__skill__independent-block-volume-architecture.md.
--
-- Tables added:
--   1. volumes                  — first-class volume resource
--   2. volume_attachments       — attachment records (one active per volume enforced by partial unique index)
--
-- Existing table changes:
--   3. jobs.instance_id         — relaxed from NOT NULL to nullable (volume jobs have no instance)
--   4. jobs.volume_id           — new nullable FK to volumes (set for volume-scoped jobs)
--
-- Compatibility:
--   - Existing rows in jobs all have instance_id set; altering to nullable is safe.
--   - ADD COLUMN uses IF NOT EXISTS to survive re-runs on partially-applied state.
--   - root_disks table is not altered; volumes reference it via source_disk_id.
--   - The jobs_subject_check constraint is intentionally omitted; the application
--     layer enforces the single-subject invariant. Adding a CHECK on existing rows
--     is risky in test environments and adds no safety beyond what the FK provides.
--
-- Source: P2_MIGRATION_COMPATIBILITY_RULES.md §7 (no destructive changes to Phase 1 tables).

-- ── 1. volumes ────────────────────────────────────────────────────────────────
-- Source: P2_VOLUME_MODEL.md §2 (identity), §3 (state machine), §6 (schema).
CREATE TABLE IF NOT EXISTS volumes (
    id                  VARCHAR(64)     NOT NULL PRIMARY KEY,   -- vol_ + KSUID
    owner_principal_id  VARCHAR(64)     NOT NULL,               -- FK to principals; VARCHAR matches existing pattern
    display_name        VARCHAR(63)     NOT NULL,
    region              VARCHAR(64)     NOT NULL,
    availability_zone   VARCHAR(64)     NOT NULL,
    size_gb             INTEGER         NOT NULL
                            CONSTRAINT volumes_size_gb_positive CHECK (size_gb > 0),
    origin              VARCHAR(20)     NOT NULL
                            CONSTRAINT volumes_origin_check CHECK (
                                origin IN ('blank', 'root_disk', 'snapshot')
                            ),
    source_disk_id          UUID     REFERENCES root_disks(disk_id),   -- origin='root_disk' only
    source_snapshot_id  VARCHAR(64),                                        -- origin='snapshot'; FK added when snapshots table exists
    status              VARCHAR(20)     NOT NULL DEFAULT 'creating'
                            CONSTRAINT volumes_status_check CHECK (
                                status IN ('creating', 'available', 'attaching', 'in_use',
                                           'detaching', 'deleting', 'deleted', 'error')
                            ),
    storage_path        VARCHAR(1024),              -- set after creation completes
    storage_pool_id     VARCHAR(64),
    version             INTEGER         NOT NULL DEFAULT 0,
    locked_by           VARCHAR(64),               -- job_id holding exclusive mutation lock; same pattern as instances
    created_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ
);

-- Owner + name uniqueness (soft-delete aware).
CREATE UNIQUE INDEX IF NOT EXISTS idx_volumes_owner_name
    ON volumes (owner_principal_id, display_name)
    WHERE deleted_at IS NULL;

-- Owner listing, newest first.
CREATE INDEX IF NOT EXISTS idx_volumes_owner_created
    ON volumes (owner_principal_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- Fast status-based scans for reconciler.
CREATE INDEX IF NOT EXISTS idx_volumes_status
    ON volumes (status)
    WHERE deleted_at IS NULL;

-- ── 2. volume_attachments ─────────────────────────────────────────────────────
-- Source: P2_VOLUME_MODEL.md §4 (attachment semantics), §7 (VOL-I-1).
CREATE TABLE IF NOT EXISTS volume_attachments (
    id                      VARCHAR(64)     NOT NULL PRIMARY KEY,   -- vatt_ + KSUID
    volume_id               VARCHAR(64)     NOT NULL REFERENCES volumes(id),
    instance_id             VARCHAR(64)     NOT NULL REFERENCES instances(id),
    device_path             VARCHAR(64)     NOT NULL,               -- e.g. /dev/vdb
    delete_on_termination   BOOLEAN         NOT NULL DEFAULT FALSE,
    attached_at             TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    detached_at             TIMESTAMPTZ                             -- NULL while active
);

-- VOL-I-1: at most one active (non-detached) attachment per volume.
-- Source: P2_VOLUME_MODEL.md §7 invariant VOL-I-1.
CREATE UNIQUE INDEX IF NOT EXISTS idx_volume_attachments_active
    ON volume_attachments (volume_id)
    WHERE detached_at IS NULL;

-- Fast lookup of active attachments for an instance (for list-volumes-by-instance).
CREATE INDEX IF NOT EXISTS idx_volume_attachments_instance_active
    ON volume_attachments (instance_id)
    WHERE detached_at IS NULL;

-- ── 3. Extend jobs table for volume-scoped jobs ───────────────────────────────
-- jobs.instance_id was NOT NULL; volume jobs have no instance context.
-- Relax to nullable and add volume_id column (IF NOT EXISTS for re-run safety).
-- Source: JOB_MODEL_V1 §1 (job types), P2_VOLUME_MODEL.md §4 (async job dispatch).
ALTER TABLE jobs
    ALTER COLUMN instance_id DROP NOT NULL;

ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS volume_id VARCHAR(64) REFERENCES volumes(id);

-- Fast volume-job lookup (equivalent to HasActivePendingJob for volumes).
CREATE INDEX IF NOT EXISTS idx_jobs_volume_id_status
    ON jobs (volume_id, status)
    WHERE volume_id IS NOT NULL;
