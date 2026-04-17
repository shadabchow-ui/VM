-- 007_dual_stack_routing.up.sql — VM-P3A Job 1: Dual-stack foundation and advanced route-table model.
--
-- This migration extends the VM networking schema to carry IPv6 alongside IPv4
-- and adds the structural primitives for the advanced route-table model required
-- by VM-P3A-1 and VM-P3A-2.
--
-- Changes:
--   1. vpcs: add cidr_ipv6 (nullable CIDR)
--   2. subnets: add cidr_ipv6 (nullable CIDR)
--   3. network_interfaces: add private_ipv6 (nullable INET) and device_index (missing from P2 schema)
--   4. route_entries: add address_family with CHECK constraint; add index
--   5. internet_gateways: new first-class resource table
--   6. subnet_route_table_associations: add UNIQUE constraint if missing
--
-- Backward compatibility:
--   - All new columns are nullable or have safe defaults; no existing rows change.
--   - NOT VALID is used on new CHECK constraints to skip re-validation of existing rows.
--   - Status CHECK on internet_gateways is a new table so NOT VALID is not needed.
--   - Existing IPv4-only VPCs and subnets remain valid (cidr_ipv6 = NULL).
--   - Existing route_entries default to address_family = 'ipv4'.
--
-- Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate",
--         vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity",
--         VM-P3A Job 1 implementation plan.
--         P2_MIGRATION_COMPATIBILITY_RULES §7 (no destructive changes to Phase 1/2 tables).

-- ── 1. vpcs: add IPv6 CIDR ────────────────────────────────────────────────────
-- cidr_ipv6: the /56 GUA block assigned to this VPC.
-- NULL for Phase 2 VPCs until explicitly set; mandatory for new dual-stack VPCs.
-- Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate" (/56 per VPC).
ALTER TABLE vpcs
    ADD COLUMN IF NOT EXISTS cidr_ipv6 CIDR;

-- ── 2. subnets: add IPv6 CIDR ─────────────────────────────────────────────────
-- cidr_ipv6: the /64 block carved from the parent VPC's /56.
-- NULL for Phase 2 subnets; mandatory for new dual-stack subnets.
-- Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate" (/64 per Subnet).
ALTER TABLE subnets
    ADD COLUMN IF NOT EXISTS cidr_ipv6 CIDR;

-- ── 3. network_interfaces: add private_ipv6 ───────────────────────────────────
-- private_ipv6: the IPv6 address assigned from the subnet's /64 block.
-- NULL for Phase 2 NICs (IPv4-only); set during allocation for dual-stack NICs.
-- Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate" (NIC must have both addresses).
ALTER TABLE network_interfaces
    ADD COLUMN IF NOT EXISTS private_ipv6 INET;

-- device_index is referenced in the domain model but may be absent from the P2 schema.
-- Adding idempotently with DEFAULT 0 (primary NIC) for existing rows.
ALTER TABLE network_interfaces
    ADD COLUMN IF NOT EXISTS device_index INTEGER NOT NULL DEFAULT 0;

-- ── 4. route_entries: add address_family ──────────────────────────────────────
-- address_family distinguishes IPv4, IPv6, and dual-stack route entries.
-- Existing rows are IPv4 routes, so DEFAULT 'ipv4' is safe and correct.
-- Source: vm-14-03__blueprint__ §future_phases "IPv6 Integration" (Egress-Only IGW for IPv6).
ALTER TABLE route_entries
    ADD COLUMN IF NOT EXISTS address_family VARCHAR(4) NOT NULL DEFAULT 'ipv4';

-- CHECK constraint on address_family. NOT VALID: existing rows all have DEFAULT 'ipv4'
-- which is valid, but we skip the table scan for performance on large deployments.
ALTER TABLE route_entries
    ADD CONSTRAINT route_entries_address_family_check
        CHECK (address_family IN ('ipv4', 'ipv6', 'dual'))
        NOT VALID;

-- Index supporting per-address-family route lookups within a route table.
-- Used by the route-table validation queries in ValidateIGWExclusivity and
-- ValidateRouteLoopFree in route_table_repo.go.
CREATE INDEX IF NOT EXISTS idx_route_entries_rtb_af
    ON route_entries (route_table_id, address_family);

-- ── 5. internet_gateways: new first-class resource ────────────────────────────
-- The Internet Gateway (IGW) is a first-class resource that must be explicitly
-- attached to a VPC to enable internet routing (0.0.0.0/0 default route).
-- Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity":
--   "A route of 0.0.0.0/0 can only target an InternetGateway resource that is
--    explicitly attached to that route table's parent VPC."
-- Source: vm-14-03__blueprint__ §implementation_decisions "explicit InternetGateway resource".
CREATE TABLE IF NOT EXISTS internet_gateways (
    id                 VARCHAR(64)  PRIMARY KEY,
    vpc_id             VARCHAR(64)  NOT NULL REFERENCES vpcs(id),
    owner_principal_id VARCHAR(64)  NOT NULL,
    status             VARCHAR(20)  NOT NULL DEFAULT 'available',
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at         TIMESTAMPTZ,

    CONSTRAINT internet_gateways_status_check
        CHECK (status IN ('available', 'deleting', 'deleted'))
);

-- One IGW per VPC: partial unique index on non-deleted rows.
-- Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity"
-- (a VPC can only have one IGW at a time; multiple would create ambiguous routing).
CREATE UNIQUE INDEX IF NOT EXISTS idx_internet_gateways_vpc_active
    ON internet_gateways (vpc_id)
    WHERE deleted_at IS NULL;

-- Fast lookup for ownership checks (principalID → IGWs).
CREATE INDEX IF NOT EXISTS idx_internet_gateways_owner
    ON internet_gateways (owner_principal_id)
    WHERE deleted_at IS NULL;

-- ── 6. subnet_route_table_associations: ensure UNIQUE on subnet_id ────────────
-- The upsert in route_table_repo.go uses ON CONFLICT (subnet_id), so this
-- constraint must exist. Adding idempotently in case it was missed in an earlier
-- migration.
-- Source: internal/db/route_table_repo.go SetSubnetRouteTableAssociation.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'subnet_route_table_associations_subnet_id_key'
          AND conrelid = 'subnet_route_table_associations'::regclass
    ) THEN
        ALTER TABLE subnet_route_table_associations
            ADD CONSTRAINT subnet_route_table_associations_subnet_id_key
                UNIQUE (subnet_id);
    END IF;
END $$;
