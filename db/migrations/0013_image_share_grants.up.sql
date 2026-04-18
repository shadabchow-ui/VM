-- 0013_image_share_grants.up.sql
--
-- VM-P3B Job 1: cross-principal private image sharing.
--
-- Adds share-grant persistence for PRIVATE images.
-- A grant records that the image owner has explicitly allowed a named
-- grantee principal (or project principal) to:
--   - see the image in GET /v1/images and GET /v1/images/{id}
--   - launch instances from the image
--
-- Ownership semantics:
--   - The image retains its owner_id.  Grants do not transfer ownership.
--   - Instances launched by a grantee are owned by the grantee, not the sharer.
--   - Revoke is forward-looking: existing instances are unaffected; future
--     launches and visibility are blocked.
--
-- Grantee referential integrity:
--   - grantee_principal_id references principals(id) with ON DELETE CASCADE.
--     When a principal (or its backing project) is deleted the grant row is
--     removed automatically — no orphaned access remains.
--
-- Source: VM_PHASE_ROADMAP.md (VM-P3B Job 1),
--         AUTH_OWNERSHIP_MODEL_V1.md §3 (ownership must not transfer),
--         P2_PROJECT_RBAC_MODEL.md §2 (project principal_id as effective scope).

CREATE TABLE image_share_grants (
    id                    VARCHAR(64)  PRIMARY KEY,
    image_id              UUID         NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    owner_principal_id    UUID         NOT NULL,  -- denormalised from images.owner_id at grant time
    grantee_principal_id  UUID         NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- An owner may only grant a given grantee once per image.
    CONSTRAINT uq_image_share_grants_image_grantee
        UNIQUE (image_id, grantee_principal_id)
);

-- Lookup: does grantee have access to this image?
CREATE INDEX idx_image_share_grants_image_grantee
    ON image_share_grants (image_id, grantee_principal_id);

-- Lookup: list all grants for an image (owner-only endpoint).
CREATE INDEX idx_image_share_grants_image_id
    ON image_share_grants (image_id);

-- Lookup: list all images shared to a grantee (for ListImagesByPrincipal extension).
CREATE INDEX idx_image_share_grants_grantee
    ON image_share_grants (grantee_principal_id);
