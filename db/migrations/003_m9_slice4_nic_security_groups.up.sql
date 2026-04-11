-- 003_m9_slice4_nic_security_groups.up.sql
-- M9 Slice 4: Instance networking integration
--
-- This migration adds:
-- 1. nic_security_groups junction table for NIC-to-SecurityGroup relationships
-- 2. subnet_ip_allocations table for VPC-aware IP allocation
--
-- Source: P2_VPC_NETWORK_CONTRACT §4.4 (multiple SGs per NIC), §5.4 (NIC schema)

-- Junction table for NIC-to-SecurityGroup many-to-many relationship
-- Source: P2_VPC_NETWORK_CONTRACT §4.4 "Multiple security groups may be assigned to a single NIC"
CREATE TABLE IF NOT EXISTS nic_security_groups (
    nic_id              UUID NOT NULL REFERENCES network_interfaces(id) ON DELETE CASCADE,
    security_group_id   UUID NOT NULL REFERENCES security_groups(id) ON DELETE CASCADE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (nic_id, security_group_id)
);

-- Index for looking up which NICs a security group is attached to
-- Used when propagating SG rule changes to host agents
CREATE INDEX IF NOT EXISTS idx_nic_security_groups_sg_id
    ON nic_security_groups (security_group_id);

-- Subnet IP allocation table for VPC-aware IPAM
-- Mirrors the Phase 1 ip_allocations pattern but keyed by subnet
-- Source: P2_VPC_NETWORK_CONTRACT §5 (NIC private IP allocation)
CREATE TABLE IF NOT EXISTS subnet_ip_allocations (
    subnet_id           UUID NOT NULL REFERENCES subnets(id) ON DELETE CASCADE,
    ip_address          INET NOT NULL,
    allocated           BOOLEAN NOT NULL DEFAULT FALSE,
    owner_instance_id   UUID REFERENCES instances(id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subnet_id, ip_address)
);

-- Index for finding available IPs in a subnet
-- SELECT FOR UPDATE SKIP LOCKED pattern
CREATE INDEX IF NOT EXISTS idx_subnet_ip_allocations_available
    ON subnet_ip_allocations (subnet_id, allocated, ip_address)
    WHERE allocated = FALSE;

-- Index for looking up allocations by instance
-- Used for IP release on instance delete
CREATE INDEX IF NOT EXISTS idx_subnet_ip_allocations_instance
    ON subnet_ip_allocations (owner_instance_id)
    WHERE owner_instance_id IS NOT NULL;

-- Function to pre-populate IP allocations when a subnet is created
-- This creates allocation slots for each IP in the subnet's CIDR
-- Excludes network address (first) and broadcast address (last)
CREATE OR REPLACE FUNCTION populate_subnet_ip_pool()
RETURNS TRIGGER AS $$
DECLARE
    cidr_net CIDR;
    ip_addr INET;
    network_addr INET;
    broadcast_addr INET;
BEGIN
    cidr_net := NEW.cidr_ipv4::CIDR;
    network_addr := network(cidr_net);
    broadcast_addr := broadcast(cidr_net);
    
    -- Generate all IPs in the CIDR range, excluding network and broadcast
    FOR ip_addr IN SELECT host(generate_series(network_addr, broadcast_addr))::INET
    LOOP
        -- Skip network address (first IP) and broadcast address (last IP)
        IF ip_addr != network_addr AND ip_addr != broadcast_addr THEN
            INSERT INTO subnet_ip_allocations (subnet_id, ip_address, allocated)
            VALUES (NEW.id, ip_addr, FALSE)
            ON CONFLICT DO NOTHING;
        END IF;
    END LOOP;
    
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Trigger to auto-populate IP pool when subnet is created
DROP TRIGGER IF EXISTS tr_populate_subnet_ip_pool ON subnets;
CREATE TRIGGER tr_populate_subnet_ip_pool
    AFTER INSERT ON subnets
    FOR EACH ROW
    EXECUTE FUNCTION populate_subnet_ip_pool();

-- Comments for documentation
COMMENT ON TABLE nic_security_groups IS 'Junction table linking NICs to their attached security groups. Multiple SGs per NIC allowed.';
COMMENT ON TABLE subnet_ip_allocations IS 'IP address pool for VPC subnets. Uses SELECT FOR UPDATE SKIP LOCKED for allocation.';
COMMENT ON FUNCTION populate_subnet_ip_pool() IS 'Auto-populates IP allocation slots when a subnet is created.';
