-- 003_m9_slice4_nic_security_groups.down.sql
-- Rollback M9 Slice 4: Instance networking integration

DROP TRIGGER IF EXISTS tr_populate_subnet_ip_pool ON subnets;
DROP FUNCTION IF EXISTS populate_subnet_ip_pool();
DROP TABLE IF EXISTS subnet_ip_allocations;
DROP TABLE IF EXISTS nic_security_groups;
