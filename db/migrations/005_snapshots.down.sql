-- 005_snapshots.down.sql — Reverses 005_snapshots.up.sql.

-- Remove snapshot_id column from jobs.
ALTER TABLE jobs DROP COLUMN IF EXISTS snapshot_id;

-- Remove FK constraint from volumes (added in up migration).
ALTER TABLE volumes DROP CONSTRAINT IF EXISTS fk_volumes_source_snapshot;

-- Drop snapshot indexes then table.
DROP INDEX IF EXISTS idx_snapshots_source_volume;
DROP INDEX IF EXISTS idx_snapshots_status;
DROP INDEX IF EXISTS idx_snapshots_owner_created;
DROP TABLE IF EXISTS snapshots;
