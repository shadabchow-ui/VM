-- 002_vpc_networking.down.sql — Drop Phase 2 VPC networking schema.
-- Drop order is the exact reverse of creation order in 002_vpc_networking.up.sql.

DROP TABLE IF EXISTS subnet_route_table_associations;
DROP TABLE IF EXISTS subnet_ip_allocations;
DROP TABLE IF EXISTS nic_security_groups;
DROP TABLE IF EXISTS network_interfaces;
DROP TABLE IF EXISTS security_group_rules;
DROP TABLE IF EXISTS security_groups;
DROP TABLE IF EXISTS route_entries;
DROP TABLE IF EXISTS route_tables;
DROP TABLE IF EXISTS subnets;
DROP TABLE IF EXISTS vpcs;
