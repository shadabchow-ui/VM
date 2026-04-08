-- 002_hosts.up.sql — M1: Host inventory and bootstrap token tables.
--
-- IDEMPOTENT: safe to apply against a fresh DB, a DB with no hosts table,
-- or a DB with a stale hosts table from a prior delivery with wrong columns.
--
-- Source: IMPLEMENTATION_PLAN_V1 §B2, 05-02-host-runtime-worker-design.md,
--         AUTH_OWNERSHIP_MODEL_V1 §6 (mTLS bootstrap flow).

-- ── hosts ─────────────────────────────────────────────────────────────────────
-- If a stale hosts table exists without the required "id" column (from a prior
-- delivery), drop it so we can recreate it with the correct schema.
-- hosts has no foreign key references from other tables (instances.host_id is
-- plain VARCHAR, not a FK constraint), so DROP is safe.
DO $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'hosts'
    ) AND NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name   = 'hosts'
          AND column_name  = 'id'
    ) THEN
        DROP TABLE hosts CASCADE;
    END IF;
END $$;

-- Persistent inventory of all physical hypervisors in the fleet.
-- id matches the CN in the host's mTLS certificate: host-{id}.
-- Source: RUNTIMESERVICE_GRPC_V1 §6, AUTH_OWNERSHIP_MODEL_V1 §6.
CREATE TABLE IF NOT EXISTS hosts (
    id                VARCHAR(64)  NOT NULL PRIMARY KEY,
    availability_zone VARCHAR(64)  NOT NULL,
    status            VARCHAR(32)  NOT NULL DEFAULT 'provisioning',
    total_cpu         INTEGER      NOT NULL CHECK (total_cpu > 0),
    total_memory_mb   INTEGER      NOT NULL CHECK (total_memory_mb > 0),
    total_disk_gb     INTEGER      NOT NULL CHECK (total_disk_gb > 0),
    used_cpu          INTEGER      NOT NULL DEFAULT 0 CHECK (used_cpu >= 0),
    used_memory_mb    INTEGER      NOT NULL DEFAULT 0 CHECK (used_memory_mb >= 0),
    used_disk_gb      INTEGER      NOT NULL DEFAULT 0 CHECK (used_disk_gb >= 0),
    agent_version     VARCHAR(64)  NOT NULL DEFAULT '',
    last_heartbeat_at TIMESTAMPTZ,
    registered_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Add columns that may be absent from an older delivery that had the right id column
-- but was missing some M1 columns. Wrapped in DO to suppress NOTICE on fresh tables.
DO $$ BEGIN
    ALTER TABLE hosts ADD COLUMN IF NOT EXISTS agent_version     VARCHAR(64)  NOT NULL DEFAULT '';
    ALTER TABLE hosts ADD COLUMN IF NOT EXISTS used_cpu          INTEGER      NOT NULL DEFAULT 0;
    ALTER TABLE hosts ADD COLUMN IF NOT EXISTS used_memory_mb    INTEGER      NOT NULL DEFAULT 0;
    ALTER TABLE hosts ADD COLUMN IF NOT EXISTS used_disk_gb      INTEGER      NOT NULL DEFAULT 0;
    ALTER TABLE hosts ADD COLUMN IF NOT EXISTS last_heartbeat_at TIMESTAMPTZ;
    ALTER TABLE hosts ADD COLUMN IF NOT EXISTS registered_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW();
    ALTER TABLE hosts ADD COLUMN IF NOT EXISTS updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW();
EXCEPTION WHEN OTHERS THEN NULL;
END $$;

-- Add status check constraint idempotently.
DO $$ BEGIN
    ALTER TABLE hosts ADD CONSTRAINT hosts_status_check CHECK (
        status IN ('provisioning','ready','draining','maintenance','offline')
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_hosts_status_az
    ON hosts (status, availability_zone);

CREATE INDEX IF NOT EXISTS idx_hosts_heartbeat_ready
    ON hosts (last_heartbeat_at)
    WHERE status = 'ready';

-- ── bootstrap_tokens ──────────────────────────────────────────────────────────
-- One-time tokens exchanged for mTLS certificates during Host Agent bootstrap.
-- Token is stored as SHA-256(raw_token) — the raw token is never persisted.
-- Source: AUTH_OWNERSHIP_MODEL_V1 §6, 05-02-host-runtime-worker-design.md §Bootstrap.
CREATE TABLE IF NOT EXISTS bootstrap_tokens (
    token_hash  VARCHAR(64)  NOT NULL PRIMARY KEY,
    host_id     VARCHAR(64)  NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ  NOT NULL,
    used        BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_bootstrap_tokens_expires
    ON bootstrap_tokens (expires_at, used);
