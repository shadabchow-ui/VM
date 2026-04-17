-- 007_dual_stack_routing.down.sql — Reversal of VM-P3A Job 1 dual-stack schema changes.
--
-- Drops only the additions made in the up migration.
-- Does not touch any Phase 1 or Phase 2 data.

DROP INDEX IF EXISTS idx_route_entries_rtb_af;

ALTER TABLE route_entries
    DROP CONSTRAINT IF EXISTS route_entries_address_family_check;

ALTER TABLE route_entries
    DROP COLUMN IF EXISTS address_family;

ALTER TABLE network_interfaces
    DROP COLUMN IF EXISTS private_ipv6;

ALTER TABLE network_interfaces
    DROP COLUMN IF EXISTS device_index;

ALTER TABLE subnets
    DROP COLUMN IF EXISTS cidr_ipv6;

ALTER TABLE vpcs
    DROP COLUMN IF EXISTS cidr_ipv6;

DROP INDEX IF EXISTS idx_internet_gateways_owner;
DROP INDEX IF EXISTS idx_internet_gateways_vpc_active;
DROP TABLE IF EXISTS internet_gateways;
