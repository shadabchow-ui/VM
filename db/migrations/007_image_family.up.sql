-- 007_image_family.up.sql — VM-P2C-P2/P4: import_url, family_name, family_version.
--
-- Adds three columns required by the image import flow (VM-P2C-P2) and the
-- family/alias abstraction (VM-P2C-P4). All changes are backward-compatible
-- with existing rows.
--
-- Depends on: 006_images.up.sql (images table must exist with all P2C-P1 columns).
--
-- Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (import_url, custom image flow),
--         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md §family_seam,
--         P2_MIGRATION_COMPATIBILITY_RULES.md §7 (no destructive changes to Phase 1 tables),
--         image_repo.go selectImageCols (column order contract).

-- ── 1. import_url ─────────────────────────────────────────────────────────────
-- Non-nil only for IMPORT-sourced images (source_type = 'IMPORT').
-- Nullable; existing rows remain NULL.
-- Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (import flow, import_url field),
--         image_repo.go ImageRow.ImportURL.
ALTER TABLE images
    ADD COLUMN IF NOT EXISTS import_url VARCHAR(2048);

-- ── 2. family_name ────────────────────────────────────────────────────────────
-- The named image family this image belongs to.
-- Nullable; NULL for images not in any family.
-- Case-sensitive match used by ResolveFamilyLatest and ResolveFamilyByVersion.
-- Source: vm-13-01__blueprint__ §family_seam, image_repo.go ImageRow.FamilyName.
ALTER TABLE images
    ADD COLUMN IF NOT EXISTS family_name VARCHAR(128);

-- ── 3. family_version ────────────────────────────────────────────────────────
-- Monotonic integer version within the family.
-- Nullable; NULL when family_name is NULL.
-- Higher value = newer preferred candidate. Resolution ordering:
--   ORDER BY family_version DESC NULLS LAST, created_at DESC
-- Source: vm-13-01__blueprint__ §family_seam, image_repo.go ResolveFamilyLatest.
ALTER TABLE images
    ADD COLUMN IF NOT EXISTS family_version INTEGER;

-- ── 4. Integrity constraint ───────────────────────────────────────────────────
-- family_version is only meaningful when family_name is set.
-- NOT VALID: skip re-checking existing rows (all have NULL family_name/version).
-- Source: P2_MIGRATION_COMPATIBILITY_RULES.md §7.
ALTER TABLE images
    ADD CONSTRAINT images_family_version_requires_name
        CHECK (family_version IS NULL OR family_name IS NOT NULL)
        NOT VALID;

-- ── 5. Index for family resolution queries ────────────────────────────────────
-- Covers both ResolveFamilyLatest and ResolveFamilyByVersion:
--   WHERE family_name = $1
--     AND status IN ('ACTIVE', 'DEPRECATED')
--     AND (visibility = 'PUBLIC' OR (visibility = 'PRIVATE' AND owner_id = $N))
-- Partial index (WHERE family_name IS NOT NULL) keeps it compact; platform images
-- with no family membership do not consume index space.
-- Source: image_repo.go ResolveFamilyLatest, ResolveFamilyByVersion.
CREATE INDEX IF NOT EXISTS idx_images_family_name_status
    ON images (family_name, status)
    WHERE family_name IS NOT NULL;
