-- 008_image_family.up.sql — VM-P2C-P3: Image family resolution indexes.
--
-- The family_name and family_version columns were added in 007_image_custom.up.sql
-- as a forward-compat seam. This migration activates them for production use by:
--
--   1. Adding a composite resolution index on (family_name, status, family_version DESC)
--      for efficient ResolveFamilyLatest queries.
--   2. Adding a unique constraint on (owner_id, family_name, family_version) so that
--      within a single owner's private family, no two images share the same version.
--      NULL family_name rows are excluded (partial index semantics on UNIQUE are not
--      supported in all PG versions, so the constraint is on non-null family_name only
--      via a partial unique index).
--   3. Adding a visibility-scoped index for PUBLIC family resolution.
--
-- All changes are additive and backward-compatible with existing rows.
-- Existing rows have family_name = NULL and are not affected by the unique index.
--
-- Source: vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md §family_seam,
--         P2_IMAGE_SNAPSHOT_MODEL.md §3 (image family ordering rules),
--         P2_MIGRATION_COMPATIBILITY_RULES.md §7 (no destructive changes).

-- ── 1. Family resolution composite index ─────────────────────────────────────
-- Used by ResolveFamilyLatest: WHERE family_name=$1 AND (visibility='PUBLIC' OR owner_id=$2)
--   AND status IN ('ACTIVE','DEPRECATED')
--   ORDER BY family_version DESC NULLS LAST, created_at DESC
-- Sparse: only rows where family_name IS NOT NULL.
CREATE INDEX IF NOT EXISTS idx_images_family_resolution
    ON images (family_name, status, family_version DESC NULLS LAST, created_at DESC)
    WHERE family_name IS NOT NULL;

-- ── 2. Per-owner family version uniqueness ────────────────────────────────────
-- Enforces: within one owner's named family, each version number is unique.
-- PUBLIC platform image families (owner_id='system') are also covered.
-- family_version = NULL rows (images without a version) are excluded from uniqueness
-- enforcement — multiple unversioned images in the same family are permitted.
-- Source: vm-13-01__blueprint__ §family_seam (version ordering must be deterministic).
CREATE UNIQUE INDEX IF NOT EXISTS idx_images_family_owner_version_unique
    ON images (owner_id, family_name, family_version)
    WHERE family_name IS NOT NULL AND family_version IS NOT NULL;

-- ── 3. Public family visibility index ─────────────────────────────────────────
-- Used when resolving PUBLIC image families (visibility='PUBLIC' scoped scans).
CREATE INDEX IF NOT EXISTS idx_images_family_public
    ON images (family_name, family_version DESC NULLS LAST, created_at DESC)
    WHERE family_name IS NOT NULL AND visibility = 'PUBLIC';
