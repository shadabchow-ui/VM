-- 0014_image_trust_fields.down.sql
--
-- Reverses 0014_image_trust_fields.up.sql.

DROP INDEX IF EXISTS idx_images_platform_signature;

ALTER TABLE images
    DROP COLUMN IF EXISTS provenance_hash,
    DROP COLUMN IF EXISTS signature_valid;
