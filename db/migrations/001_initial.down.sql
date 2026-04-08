-- 001_initial.down.sql — Drop all M0 schema objects.
-- Drop order is the exact reverse of creation order in 001_initial.up.sql.
-- Source: DAY_1_FILE_CREATION_PLAN_V1 §C3.

DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS instance_events;
DROP TABLE IF EXISTS ssh_public_keys;
DROP TABLE IF EXISTS ip_allocations;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS root_disks;
DROP TABLE IF EXISTS instances;
DROP TABLE IF EXISTS instance_types;
DROP TABLE IF EXISTS images;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS principals;
