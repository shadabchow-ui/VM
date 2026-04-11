-- 003_cross_host_networking.down.sql — Rollback M9 Slice 4 cross-host networking groundwork.

DROP TABLE IF EXISTS nic_vtep_registrations;
DROP TABLE IF EXISTS vni_allocations;
DROP TABLE IF EXISTS vpc_host_attachments;
DROP TABLE IF EXISTS host_tunnel_endpoints;
