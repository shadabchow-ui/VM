-- 019_vm_boot_lane_ssh_key.up.sql — Add ssh_key_name to instances table.
--
-- Phase 1 boot lane: persist the SSH key name chosen at instance creation
-- so the worker can resolve it to the public key and inject it into the
-- cloud-init config-drive metadata seed during provisioning.
--
-- Source: VM-RUNTIME-BOOT-LANE-PHASE-A-B-C job specification.

ALTER TABLE instances ADD COLUMN ssh_key_name VARCHAR(255);
