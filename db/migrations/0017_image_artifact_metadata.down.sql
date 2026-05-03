-- 0017_image_artifact_metadata.down.sql

ALTER TABLE images DROP COLUMN IF EXISTS format;
ALTER TABLE images DROP COLUMN IF EXISTS size_bytes;
ALTER TABLE images DROP COLUMN IF EXISTS image_digest;
ALTER TABLE images DROP COLUMN IF EXISTS validation_error;
