-- 001_initial.up.sql — M0 initial schema migration.
--
-- Source: INSTANCE_MODEL_V1, JOB_MODEL_V1, AUTH_OWNERSHIP_MODEL_V1,
--         IP_ALLOCATION_CONTRACT_V1, EVENTS_SCHEMA_V1, core-architecture-blueprint.md §schema.
--
-- Table creation order respects FK dependencies.
-- Seed data appended at the end (R-04: seed in same file, not separate migration).

-- ── 1. principals ─────────────────────────────────────────────────────────────
-- Ownership anchor. Phase 1: principal_type='ACCOUNT'. Phase 2: adds 'PROJECT'.
-- Source: AUTH_OWNERSHIP_MODEL_V1 §3, R-12.
CREATE TABLE principals (
    id             UUID         NOT NULL PRIMARY KEY,
    principal_type VARCHAR(20)  NOT NULL DEFAULT 'ACCOUNT',
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT principals_type_check CHECK (principal_type IN ('ACCOUNT', 'PROJECT'))
);

-- ── 2. accounts ───────────────────────────────────────────────────────────────
CREATE TABLE accounts (
    id           UUID         NOT NULL PRIMARY KEY,
    principal_id UUID         NOT NULL UNIQUE REFERENCES principals(id),
    email        VARCHAR(255) NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- ── 3. images ─────────────────────────────────────────────────────────────────
-- Platform-managed base images. Source: INSTANCE_MODEL_V1 §7.
CREATE TABLE images (
    id           UUID         NOT NULL PRIMARY KEY,
    name         VARCHAR(255) NOT NULL,
    description  TEXT,
    os_family    VARCHAR(64)  NOT NULL,   -- e.g. ubuntu, debian
    os_version   VARCHAR(32)  NOT NULL,   -- e.g. 22.04, 12
    architecture VARCHAR(16)  NOT NULL DEFAULT 'x86_64',
    storage_url  TEXT         NOT NULL,   -- object storage URL for rootfs
    is_public    BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- ── 4. instance_types ─────────────────────────────────────────────────────────
-- Shape catalog. Source: INSTANCE_MODEL_V1 §6, 02-03-instance-shapes.md.
CREATE TABLE instance_types (
    id          VARCHAR(64)  NOT NULL PRIMARY KEY,   -- e.g. c1.small
    cpu_cores   INTEGER      NOT NULL CHECK (cpu_cores > 0),
    memory_mb   INTEGER      NOT NULL CHECK (memory_mb > 0),
    disk_gb     INTEGER      NOT NULL CHECK (disk_gb > 0),
    description TEXT,
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE
);

-- ── 5. instances ──────────────────────────────────────────────────────────────
-- Core instance record. Source: INSTANCE_MODEL_V1 §2, §4, R-11, R-12, R-14.
CREATE TABLE instances (
    id                 VARCHAR(64)  NOT NULL PRIMARY KEY,   -- KSUID, R-11: never reused
    name               VARCHAR(255) NOT NULL,
    owner_principal_id UUID         NOT NULL REFERENCES principals(id),  -- R-12
    vm_state           VARCHAR(32)  NOT NULL DEFAULT 'requested',
    instance_type_id   VARCHAR(64)  NOT NULL REFERENCES instance_types(id),
    image_id           UUID         NOT NULL REFERENCES images(id),
    host_id            VARCHAR(64),                          -- NULL until provisioned
    availability_zone  VARCHAR(64)  NOT NULL,
    version            INTEGER      NOT NULL DEFAULT 0,      -- optimistic lock
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at         TIMESTAMPTZ,                          -- soft delete
    CONSTRAINT instances_state_check CHECK (
        vm_state IN (
            'requested','provisioning','running',
            'stopping','stopped','starting',
            'rebooting','deleting','deleted','failed'
        )
    )
);

CREATE INDEX idx_instances_owner_created
    ON instances (owner_principal_id, created_at DESC);

CREATE INDEX idx_instances_state_updated
    ON instances (vm_state, updated_at ASC);

CREATE INDEX idx_instances_host_state
    ON instances (host_id, vm_state)
    WHERE host_id IS NOT NULL;

CREATE INDEX idx_instances_not_deleted
    ON instances (deleted_at)
    WHERE deleted_at IS NULL;

-- ── 6. root_disks ─────────────────────────────────────────────────────────────
-- Source: INSTANCE_MODEL_V1 §8, R-14 (separate table, not embedded in instances).
CREATE TABLE root_disks (
    disk_id              UUID         NOT NULL PRIMARY KEY,
    instance_id          VARCHAR(64)  REFERENCES instances(id),  -- NULL if detached
    source_image_id      UUID         NOT NULL REFERENCES images(id),
    storage_pool_id      UUID         NOT NULL,
    storage_path         TEXT         NOT NULL,   -- nfs://filer/vol/disk_id.qcow2
    size_gb              INTEGER      NOT NULL CHECK (size_gb > 0),
    delete_on_termination BOOLEAN     NOT NULL DEFAULT TRUE,  -- Phase 1: always true (R-15)
    status               VARCHAR(32)  NOT NULL DEFAULT 'CREATING',
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT root_disks_status_check CHECK (status IN ('CREATING','ATTACHED','DETACHED'))
);

-- ── 7. jobs ───────────────────────────────────────────────────────────────────
-- Async job queue. Source: JOB_MODEL_V1 §1.
CREATE TABLE jobs (
    id              VARCHAR(64)  NOT NULL PRIMARY KEY,
    instance_id     VARCHAR(64)  NOT NULL REFERENCES instances(id),
    job_type        VARCHAR(32)  NOT NULL,
    status          VARCHAR(32)  NOT NULL DEFAULT 'pending',
    idempotency_key VARCHAR(255) NOT NULL UNIQUE,
    attempt_count   INTEGER      NOT NULL DEFAULT 0,
    max_attempts    INTEGER      NOT NULL DEFAULT 3,
    error_message   TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    claimed_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    CONSTRAINT jobs_status_check CHECK (
        status IN ('pending','in_progress','completed','failed','dead')
    ),
    CONSTRAINT jobs_type_check CHECK (
        job_type IN (
            'INSTANCE_CREATE','INSTANCE_DELETE',
            'INSTANCE_START','INSTANCE_STOP','INSTANCE_REBOOT'
        )
    )
);

-- Worker poll query: FIFO pending jobs. Source: JOB_MODEL_V1 §SELECT FOR UPDATE SKIP LOCKED.
CREATE INDEX idx_jobs_pending
    ON jobs (status, created_at ASC)
    WHERE status = 'pending';

-- ── 8. idempotency_keys ───────────────────────────────────────────────────────
-- Source: JOB_MODEL_V1 §idempotency, 03-02-async-job-model §idempotency.
CREATE TABLE idempotency_keys (
    key           VARCHAR(255) NOT NULL PRIMARY KEY,
    job_id        VARCHAR(64)  NOT NULL REFERENCES jobs(id),
    response_code INTEGER      NOT NULL,
    response_body JSONB        NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
    -- TTL: rows older than 24 hours purged by a background job.
);

-- ── 9. ip_allocations ─────────────────────────────────────────────────────────
-- Source: IP_ALLOCATION_CONTRACT_V1 §2, core-architecture-blueprint §ip_allocations.
-- Atomic allocation via SELECT FOR UPDATE. UNIQUE constraint enforces IP uniqueness invariant (I-2).
CREATE TABLE ip_allocations (
    ip_address          INET         NOT NULL,
    vpc_id              UUID         NOT NULL,
    allocated           BOOLEAN      NOT NULL DEFAULT FALSE,
    owner_instance_id   VARCHAR(64)  REFERENCES instances(id),
    PRIMARY KEY (ip_address, vpc_id),
    UNIQUE (vpc_id, ip_address)
);

CREATE INDEX idx_ip_allocations_available
    ON ip_allocations (vpc_id, allocated)
    WHERE allocated = FALSE;

-- ── 10. ssh_public_keys ───────────────────────────────────────────────────────
-- Source: AUTH_OWNERSHIP_MODEL_V1 §5.
CREATE TABLE ssh_public_keys (
    id           UUID         NOT NULL PRIMARY KEY,
    principal_id UUID         NOT NULL REFERENCES principals(id),
    name         VARCHAR(255) NOT NULL,
    public_key   TEXT         NOT NULL,
    fingerprint  VARCHAR(255) NOT NULL,
    key_type     VARCHAR(50)  NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ssh_keys_unique_name UNIQUE (principal_id, name),
    CONSTRAINT ssh_keys_type_check CHECK (
        key_type IN ('ssh-ed25519','ecdsa-sha2-nistp256','ecdsa-sha2-nistp384','ecdsa-sha2-nistp521')
    )
);

-- ── 11. instance_events ───────────────────────────────────────────────────────
-- Source: EVENTS_SCHEMA_V1 §2, §4. Retention: last 100 per instance.
CREATE TABLE instance_events (
    id          UUID         NOT NULL PRIMARY KEY,
    instance_id VARCHAR(64)  NOT NULL REFERENCES instances(id),
    event_type  VARCHAR(64)  NOT NULL,
    message     TEXT,
    actor       VARCHAR(255),            -- principal_id or 'system'
    details     JSONB,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_instance_events_instance_time
    ON instance_events (instance_id, created_at DESC);

-- ── 12. api_keys ─────────────────────────────────────────────────────────────
-- Source: AUTH_OWNERSHIP_MODEL_V1 §2. M5 enforcement. Schema locked at M0.
CREATE TABLE api_keys (
    access_key_id         VARCHAR(64)  NOT NULL PRIMARY KEY,
    principal_id          UUID         NOT NULL REFERENCES principals(id),
    secret_key_encrypted  BYTEA        NOT NULL,
    secret_key_dek_encrypted BYTEA     NOT NULL,
    status                VARCHAR(20)  NOT NULL DEFAULT 'ACTIVE',
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_used_at          TIMESTAMPTZ,
    CONSTRAINT api_keys_status_check CHECK (status IN ('ACTIVE','INACTIVE'))
);

CREATE INDEX idx_api_keys_principal
    ON api_keys (principal_id);

-- ── Seed data ─────────────────────────────────────────────────────────────────
-- R-04: seed data in initial migration, not a separate file.

-- Default VPC ID used for Phase 1 IP pool.
-- All Phase 1 instances share a single VPC. Source: 07-01-phase-1-network-architecture.md.
DO $$ DECLARE default_vpc UUID := '00000000-0000-0000-0000-000000000001'; BEGIN

-- Instance types. Source: INSTANCE_MODEL_V1 §6, 02-03-instance-shapes.md.
INSERT INTO instance_types (id, cpu_cores, memory_mb, disk_gb, description) VALUES
    ('c1.small',   2,   4096,  50,  '2 vCPU, 4 GB RAM, 50 GB disk'),
    ('c1.medium',  4,   8192,  100, '4 vCPU, 8 GB RAM, 100 GB disk'),
    ('c1.large',   8,  16384,  200, '8 vCPU, 16 GB RAM, 200 GB disk'),
    ('c1.xlarge', 16,  32768,  500, '16 vCPU, 32 GB RAM, 500 GB disk')
ON CONFLICT (id) DO NOTHING;

-- Platform images. Source: IMPLEMENTATION_PLAN_V1 §26 (Ubuntu 22.04, Debian 12).
INSERT INTO images (id, name, description, os_family, os_version, storage_url) VALUES
    ('00000000-0000-0000-0000-000000000010',
     'ubuntu-22.04-lts', 'Ubuntu 22.04 LTS (Jammy Jellyfish)',
     'ubuntu', '22.04',
     'object://images/ubuntu-22.04-base.qcow2'),
    ('00000000-0000-0000-0000-000000000011',
     'debian-12', 'Debian 12 (Bookworm)',
     'debian', '12',
     'object://images/debian-12-base.qcow2')
ON CONFLICT (id) DO NOTHING;

-- Default system principal. Used for internal operations.
-- Source: AUTH_OWNERSHIP_MODEL_V1 §3.
INSERT INTO principals (id, principal_type) VALUES
    ('00000000-0000-0000-0000-000000000001', 'ACCOUNT')
ON CONFLICT (id) DO NOTHING;

INSERT INTO accounts (id, principal_id, email) VALUES
    ('00000000-0000-0000-0000-000000000001',
     '00000000-0000-0000-0000-000000000001',
     'system@compute-platform.internal')
ON CONFLICT (id) DO NOTHING;

-- IP pool: RFC 1918 private range for Phase 1.
-- Source: IP_ALLOCATION_CONTRACT_V1, 07-01-phase-1-network-architecture.md.
-- Seeding 10.0.0.1 – 10.0.0.254 (254 addresses) in the default VPC.
INSERT INTO ip_allocations (ip_address, vpc_id, allocated)
SELECT
    ('10.0.0.' || g)::INET,
    default_vpc,
    FALSE
FROM generate_series(1, 254) g
ON CONFLICT DO NOTHING;

END $$;
