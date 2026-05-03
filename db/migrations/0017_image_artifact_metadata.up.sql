-- 0017_image_artifact_metadata.up.sql
--
-- VM-TRUSTED-IMAGE-FACTORY-PHASE-J: artifact metadata for trusted image factory MVP.
--
-- Adds columns to images table for the image validation and publish pipeline:
--
--   format            VARCHAR(20)  — disk format (qcow2, raw, vmdk). NULL until validated.
--   size_bytes        BIGINT       — actual image artifact size in bytes. NULL until imported.
--   image_digest      VARCHAR(255) — content-addressed digest of the image artifact (sha256:<hex>).
--                                    Independent from image_build_manifests.image_digest;
--                                    this is the final validated digest on the images row.
--   validation_error  TEXT         — last validation error message. NULL on success or not yet run.
--                                    Populated when a validation stage fails; cleared on re-validation.
--
-- Source: combined_vm-13-01__blueprint__trusted-image-factory-and-validation-pipeline.md
--             (artifact metadata and validation pipeline),
--         P2_IMAGE_SNAPSHOT_MODEL.md §3.

ALTER TABLE images
    ADD COLUMN IF NOT EXISTS format VARCHAR(20) NULL;

ALTER TABLE images
    ADD COLUMN IF NOT EXISTS size_bytes BIGINT NULL;

ALTER TABLE images
    ADD COLUMN IF NOT EXISTS image_digest VARCHAR(255) NULL;

ALTER TABLE images
    ADD COLUMN IF NOT EXISTS validation_error TEXT NULL;
