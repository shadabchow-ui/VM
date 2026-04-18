-- 0014_image_trust_fields.up.sql
--
-- VM-P3B Job 2: image catalog and admission policy core.
--
-- Adds the platform trust boundary seam to the images table:
--
--   provenance_hash  VARCHAR(255) NULL
--     SHA-256 or in-toto attestation digest of the SLSA L3 provenance record
--     produced by the trusted image factory for this image.
--     Non-null only for source_type = 'PLATFORM' (owner = system).
--     NULL for user-created (SNAPSHOT / IMPORT) images — no factory provenance.
--
--   signature_valid  BOOLEAN NULL
--     Set by the image validation worker after verifying the factory signature
--     against the provenance_hash using the platform's public key.
--     TRUE  = signature verified.
--     FALSE = signature check ran and failed (image must not be launched).
--     NULL  = not yet checked (PENDING_VALIDATION state, or non-platform image).
--
-- Admission rule (vm-13-01__blueprint__ §core_contracts):
--   "The VM admission controller MUST verify the cryptographic signature of any
--    image with owner: platform. It MUST NOT attempt to verify signatures for
--    images with owner: project_id (custom images)."
--
-- The admission controller enforces:
--   PLATFORM images: signature_valid must be TRUE.
--   Non-PLATFORM images: signature_valid is ignored (field may be NULL).
--
-- Source: vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md
--             §core_contracts "Platform Trust Boundary",
--         P2_IMAGE_SNAPSHOT_MODEL.md §3,
--         INSTANCE_MODEL_V1.md §7.

ALTER TABLE images
    ADD COLUMN IF NOT EXISTS provenance_hash VARCHAR(255) NULL,
    ADD COLUMN IF NOT EXISTS signature_valid BOOLEAN NULL;

-- Index for quick admission-time lookup of platform images with invalid/missing signatures.
-- Used by monitoring queries; admission itself filters on a single image by ID.
CREATE INDEX IF NOT EXISTS idx_images_platform_signature
    ON images (source_type, signature_valid)
    WHERE source_type = 'PLATFORM';
