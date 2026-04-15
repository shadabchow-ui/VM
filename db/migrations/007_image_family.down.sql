-- 007_image_family.down.sql — rollback for 007_image_family.up.sql.
DROP INDEX IF EXISTS idx_images_family_name_status;
ALTER TABLE images DROP CONSTRAINT IF EXISTS images_family_version_requires_name;
ALTER TABLE images DROP COLUMN IF EXISTS family_version;
ALTER TABLE images DROP COLUMN IF EXISTS family_name;
ALTER TABLE images DROP COLUMN IF EXISTS import_url;
