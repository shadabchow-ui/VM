-- 008_image_family.down.sql — Reverses 008_image_family.up.sql.
DROP INDEX IF EXISTS idx_images_family_public;
DROP INDEX IF EXISTS idx_images_family_owner_version_unique;
DROP INDEX IF EXISTS idx_images_family_resolution;
