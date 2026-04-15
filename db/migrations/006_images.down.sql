-- 006_images.down.sql — Rollback for 006_images.up.sql.
--
-- Reverses all changes made by 006_images.up.sql in reverse order.
-- Source: P2_MIGRATION_COMPATIBILITY_RULES.md §7.

DROP INDEX IF EXISTS idx_images_source_type;
DROP INDEX IF EXISTS idx_images_owner_visibility;
DROP INDEX IF EXISTS idx_images_status;

ALTER TABLE images DROP CONSTRAINT IF EXISTS images_validation_status_check;
ALTER TABLE images DROP COLUMN IF EXISTS validation_status;

ALTER TABLE images DROP COLUMN IF EXISTS source_snapshot_id;

ALTER TABLE images DROP CONSTRAINT IF EXISTS images_status_check;

ALTER TABLE images DROP COLUMN IF EXISTS obsoleted_at;
ALTER TABLE images DROP COLUMN IF EXISTS deprecated_at;

ALTER TABLE images DROP COLUMN IF EXISTS updated_at;
