-- M1 rollback: drop in reverse dependency order.
DROP INDEX IF EXISTS idx_bootstrap_tokens_expires;
DROP TABLE IF EXISTS bootstrap_tokens;

DROP INDEX IF EXISTS idx_hosts_heartbeat_ready;
DROP INDEX IF EXISTS idx_hosts_status_az;
DROP TABLE IF EXISTS hosts;
