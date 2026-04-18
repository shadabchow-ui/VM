-- 008_eip_nat_gateway.down.sql — Reversal of VM-P3A Job 2 schema additions.

DROP INDEX IF EXISTS idx_nat_gateways_owner;
DROP INDEX IF EXISTS idx_nat_gateways_eip_active;
DROP INDEX IF EXISTS idx_nat_gateways_vpc;
DROP INDEX IF EXISTS idx_nat_gateways_subnet_active;
DROP TABLE IF EXISTS nat_gateways;

DROP INDEX IF EXISTS idx_elastic_ips_associated_resource;
DROP INDEX IF EXISTS idx_elastic_ips_owner;
DROP INDEX IF EXISTS idx_elastic_ips_public_ip_active;
DROP TABLE IF EXISTS elastic_ips;
