-- 0015_image_validation_results.up.sql
--
-- VM-P3B Job 3: trusted image factory, validation, and rollout seams.
--
-- Creates three tables:
--
--  image_build_manifests   — build manifest / digest / provenance metadata
--                            produced by the Image Build Service.
--
--  image_validation_results — per-stage validation results (boot, health,
--                             security, integrity) recorded by the
--                             Image Validation Service worker.
--
--  image_cve_waivers       — CVE waiver records consulted by the Validation
--                            Service when deciding pass/fail for security stages.
--
-- These tables are append-only from the control-plane perspective.
-- State transitions on the parent images row are driven by the existing
-- UpdateImageValidationStatus method in image_repo.go.
--
-- Source: vm-13-01__blueprint__ §Image Build Service,
--                               §Image Signing and Provenance Service,
--                               §Image Validation Service,
--         vm-13-01__skill__ §instructions,
--         P2_IMAGE_SNAPSHOT_MODEL.md §3.

-- ── image_build_manifests ─────────────────────────────────────────────────────
--
-- Records the build manifest and signing provenance produced by the trusted
-- image factory for each PLATFORM image. One row per image (1:1 with images
-- where source_type = 'PLATFORM').
--
-- Fields:
--   image_id          FK → images.id (the image this manifest describes)
--   build_config_ref  Source repo commit / ref used by the hermetic builder
--   base_image_digest Content-addressed digest of the base image used in build
--   image_digest      Content-addressed digest of the produced image artifact
--                     (e.g. sha256:<hex>). Used as the stable identity for signing.
--   provenance_json   Signed SLSA L3 in-toto attestation (JSON, signed by KMS).
--                     NULL until the signing service has produced the attestation.
--   signature         Detached cryptographic signature over image_digest,
--                     produced by the HSM/KMS-backed signing service.
--                     NULL until signing is complete.
--   signed_at         Timestamp when signing was completed. NULL until signed.
--   created_at        When this manifest record was created.

CREATE TABLE image_build_manifests (
    image_id          UUID         PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    build_config_ref  VARCHAR(512) NOT NULL,
    base_image_digest VARCHAR(255) NOT NULL,
    image_digest      VARCHAR(255) NOT NULL,
    provenance_json   TEXT         NULL,  -- signed SLSA L3 attestation; NULL until signed
    signature         TEXT         NULL,  -- detached signature over image_digest; NULL until signed
    signed_at         TIMESTAMPTZ  NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- ── image_validation_results ──────────────────────────────────────────────────
--
-- Records the outcome of each validation stage run against a candidate image.
-- The Image Validation Service worker inserts one row per stage per attempt.
--
-- Stage values (per vm-13-01__blueprint__ §Image Validation Service):
--   'boot'       — VM boots successfully and agent responds
--   'health'     — Agent health checks pass
--   'security'   — CVE scan (may check image_cve_waivers for exceptions)
--   'integrity'  — File-system / package integrity checks
--   'performance'— (future phase) Boot time and baseline performance
--
-- Result values: 'pass' | 'fail'
--
-- detail_json: optional JSON blob with stage-specific output (scan results,
-- error messages, etc.). Stored for auditability; not surfaced in the API.

CREATE TABLE image_validation_results (
    id           VARCHAR(64)  PRIMARY KEY,  -- idgen prefix "ivr"
    image_id     UUID         NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    job_id       VARCHAR(64)  NOT NULL,     -- the IMAGE_VALIDATE job that ran this stage
    stage        VARCHAR(32)  NOT NULL,     -- 'boot' | 'health' | 'security' | 'integrity'
    result       VARCHAR(8)   NOT NULL,     -- 'pass' | 'fail'
    detail_json  TEXT         NULL,         -- optional audit payload; not in API
    recorded_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_ivr_stage  CHECK (stage  IN ('boot','health','security','integrity','performance')),
    CONSTRAINT chk_ivr_result CHECK (result IN ('pass','fail'))
);

-- Lookup: all results for a given image (used by promotion gate check).
CREATE INDEX idx_ivr_image_id ON image_validation_results (image_id);
-- Lookup: all results for a given job (used by the validation worker).
CREATE INDEX idx_ivr_job_id   ON image_validation_results (job_id);

-- ── image_cve_waivers ─────────────────────────────────────────────────────────
--
-- CVE waiver records allowing the security validation stage to pass for known
-- CVEs that have been explicitly reviewed and accepted.
--
-- The Image Validation Service worker looks up waivers before failing on a CVE
-- finding. A waiver permits one named CVE ID for one named image family
-- (or globally if image_family IS NULL).
--
-- Fields:
--   cve_id        CVE identifier (e.g. "CVE-2024-12345")
--   image_family  If non-NULL, waiver applies only to that image family.
--                 If NULL, waiver applies globally.
--   granted_by    Principal ID of the operator who granted the waiver.
--   reason        Mandatory justification text for the audit trail.
--   expires_at    NULL = permanent waiver; non-NULL = waiver expires at this time.
--   revoked_at    NULL = active; non-NULL = waiver was revoked (soft delete).
--   created_at    When the waiver was created.
--
-- Source: vm-13-01__blueprint__ §future_phases "Operational Hardening"
--         (CVE Waiver Database),
--         vm-13-01__skill__ §instructions "Formalize a CVE Waiver Process".

CREATE TABLE image_cve_waivers (
    id            VARCHAR(64)  PRIMARY KEY,   -- idgen prefix "cve"
    cve_id        VARCHAR(64)  NOT NULL,
    image_family  VARCHAR(255) NULL,          -- NULL = global waiver
    granted_by    UUID         NOT NULL,
    reason        TEXT         NOT NULL,
    expires_at    TIMESTAMPTZ  NULL,
    revoked_at    TIMESTAMPTZ  NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Lookup: active waiver for a specific CVE + family.
CREATE INDEX idx_cve_waivers_cve_family
    ON image_cve_waivers (cve_id, image_family)
    WHERE revoked_at IS NULL;
