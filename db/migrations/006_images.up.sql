-- 006_images.up.sql — VM-P2C-P1: Image model lifecycle and launch admission foundations.
--
-- The Phase 1 `images` table was seeded in 001_initial.up.sql with these columns:
--   id UUID PK, name, os_family, os_version, architecture, owner_id UUID,
--   visibility VARCHAR(20) DEFAULT 'PUBLIC', source_type VARCHAR(20) DEFAULT 'PLATFORM',
--   storage_url VARCHAR(1024), min_disk_gb INTEGER, status VARCHAR(20) DEFAULT 'ACTIVE',
--   created_at TIMESTAMPTZ.
--
-- This migration:
--   1. Adds `updated_at` for optimistic-lock-compatible tracking.
--   2. Adds `deprecated_at` and `obsoleted_at` timestamps to record lifecycle transitions.
--   3. Adds a CHECK constraint on `status` enforcing the canonical five-state enum.
--   4. Adds `source_snapshot_id` FK for Phase 2 custom-image-from-snapshot path.
--   5. Adds `validation_status` for Phase 2 image validation pipeline.
--   6. Adds indexes for owner+status list queries and admission lookups.
--
-- All changes are backward-compatible with Phase 1 seed rows:
--   - `updated_at` DEFAULT NOW() fills existing rows.
--   - `deprecated_at` / `obsoleted_at` are nullable; existing rows remain NULL.
--   - Status CHECK uses NOT VALID to skip existing rows (all are 'ACTIVE', which is valid).
--   - `source_snapshot_id` is nullable; existing platform-image rows remain NULL.
--   - `validation_status` DEFAULT 'passed' covers all existing platform images.
--
-- Source: INSTANCE_MODEL_V1.md §7 (images schema),
--         P2_IMAGE_SNAPSHOT_MODEL.md §3 (custom image lifecycle, status enum),
--         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md (state machine),
--         P2_MIGRATION_COMPATIBILITY_RULES.md §7 (no destructive changes to Phase 1 tables).

-- ── 1. updated_at ─────────────────────────────────────────────────────────────
-- Phase 1 images table had no updated_at. Add with DEFAULT NOW() for existing rows.
ALTER TABLE images
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- ── 2. Lifecycle transition timestamps ────────────────────────────────────────
-- deprecated_at: set when status transitions to DEPRECATED.
-- obsoleted_at:  set when status transitions to OBSOLETE.
-- Source: vm-13-01__blueprint__ §core_contracts "Image Lifecycle State Enforcement".
ALTER TABLE images
    ADD COLUMN IF NOT EXISTS deprecated_at TIMESTAMPTZ;

ALTER TABLE images
    ADD COLUMN IF NOT EXISTS obsoleted_at TIMESTAMPTZ;

-- ── 3. Status CHECK constraint ────────────────────────────────────────────────
-- Five canonical lifecycle states, derived from vm-13-01__skill__ §instructions
-- and P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
--
-- ACTIVE         — validated, launchable.
-- DEPRECATED     — still launchable; replacement available.
-- OBSOLETE       — blocked from launch; security/EOL reason.
-- FAILED         — blocked from launch; validation or pipeline failure.
-- PENDING_VALIDATION — image registered but not yet validated.
--
-- NOT VALID: does not recheck existing rows. All Phase 1 seed rows are 'ACTIVE'.
-- Source: vm-13-01__blueprint__ §core_contracts, P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
ALTER TABLE images
    ADD CONSTRAINT images_status_check
        CHECK (status IN ('ACTIVE', 'DEPRECATED', 'OBSOLETE', 'FAILED', 'PENDING_VALIDATION'))
        NOT VALID;

-- ── 4. source_snapshot_id FK (Phase 2 custom image from snapshot) ─────────────
-- Nullable; only set when source_type = 'SNAPSHOT'.
-- FK to snapshots table added with NOT VALID to skip existing NULL rows.
-- Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.10.
ALTER TABLE images
    ADD COLUMN IF NOT EXISTS source_snapshot_id VARCHAR(64) REFERENCES snapshots(id);

-- ── 5. validation_status (Phase 2 validation pipeline) ───────────────────────
-- Tracks validation pipeline state separate from the user-visible lifecycle state.
-- DEFAULT 'passed' for all existing platform images — they are pre-validated.
-- Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.3 (validation_status field).
ALTER TABLE images
    ADD COLUMN IF NOT EXISTS validation_status VARCHAR(20) NOT NULL DEFAULT 'passed';

ALTER TABLE images
    ADD CONSTRAINT images_validation_status_check
        CHECK (validation_status IN ('pending', 'validating', 'passed', 'failed'))
        NOT VALID;

-- ── 6. Indexes ────────────────────────────────────────────────────────────────

-- Admission lookup: status-only index — used by the create-instance admission check.
-- SELECT id, status, visibility, owner_id FROM images WHERE id = $1 AND status NOT IN (...)
-- Source: vm-13-01__blueprint__ §core_contracts "admission controller must consult this service".
CREATE INDEX IF NOT EXISTS idx_images_status
    ON images (status);

-- Owner-scoped listing: owner_id + visibility for GET /v1/images per-principal queries.
-- Covers both PUBLIC images (any caller) and PRIVATE images (owner only).
-- Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.7 (visibility and ownership).
CREATE INDEX IF NOT EXISTS idx_images_owner_visibility
    ON images (owner_id, visibility);

-- Source_type index: used for platform-image-only scans and operator tooling.
CREATE INDEX IF NOT EXISTS idx_images_source_type
    ON images (source_type);
