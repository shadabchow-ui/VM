-- 008_eip_nat_gateway.up.sql — VM-P3A Job 2: Public connectivity maturity.
--
-- Adds:
--   1. elastic_ips — Elastic IP (EIP) resource table.
--   2. nat_gateways — NAT Gateway resource table.
--
-- Backward compatibility:
--   - Both tables are new; no existing rows affected.
--   - elastic_ips.association_type defaults to 'none' (unassociated at creation).
--   - nat_gateways.status defaults to 'pending' to be consistent with async lifecycle.
--
-- Ownership:
--   - EIPs are owner-principal-scoped.
--   - NAT Gateways are owner-principal-scoped, subnet-scoped, and require one EIP.
--   - An EIP can be associated to one resource at a time (nic OR nat_gateway).
--
-- Source: vm-14-03__blueprint__ §core_contracts
--   "Elastic IP Allocation and Association",
--   "NAT Gateway Lifecycle",
--   "Public Connectivity Contract".

-- ── 1. elastic_ips ────────────────────────────────────────────────────────────
--
-- An Elastic IP is a first-class, owner-scoped public IPv4 address resource.
-- It is allocated from the platform's public IP pool and can be associated to
-- either a NIC (for direct instance public connectivity) or a NAT Gateway
-- (for outbound NAT from a private subnet).
--
-- association_type: 'none' | 'nic' | 'nat_gateway'
-- associated_resource_id: NIC ID or NAT Gateway ID; NULL when unassociated.
--
-- Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Allocation and Association".
CREATE TABLE IF NOT EXISTS elastic_ips (
    id                      VARCHAR(64)  PRIMARY KEY,
    owner_principal_id      VARCHAR(64)  NOT NULL,
    public_ip               INET         NOT NULL,
    association_type        VARCHAR(20)  NOT NULL DEFAULT 'none',
    associated_resource_id  VARCHAR(64),
    status                  VARCHAR(20)  NOT NULL DEFAULT 'available',
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at              TIMESTAMPTZ,

    CONSTRAINT elastic_ips_association_type_check
        CHECK (association_type IN ('none', 'nic', 'nat_gateway')),

    CONSTRAINT elastic_ips_status_check
        CHECK (status IN ('available', 'associated', 'releasing', 'released'))
);

-- One public IP must be globally unique among non-deleted EIPs.
-- The platform does not allow the same public IP to exist in two EIP records.
CREATE UNIQUE INDEX IF NOT EXISTS idx_elastic_ips_public_ip_active
    ON elastic_ips (public_ip)
    WHERE deleted_at IS NULL;

-- Owner-scoped listing.
CREATE INDEX IF NOT EXISTS idx_elastic_ips_owner
    ON elastic_ips (owner_principal_id)
    WHERE deleted_at IS NULL;

-- Association lookup: find EIP by associated resource (NIC or NAT GW).
CREATE INDEX IF NOT EXISTS idx_elastic_ips_associated_resource
    ON elastic_ips (associated_resource_id)
    WHERE association_type != 'none' AND deleted_at IS NULL;

-- ── 2. nat_gateways ───────────────────────────────────────────────────────────
--
-- A NAT Gateway enables instances in a private subnet to initiate outbound
-- internet traffic without being directly reachable from the internet.
-- It is subnet-scoped (one per subnet) and requires one EIP for outbound SNAT.
--
-- Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Lifecycle",
--         §implementation_decisions "NAT Gateway requires EIP".
CREATE TABLE IF NOT EXISTS nat_gateways (
    id                  VARCHAR(64)  PRIMARY KEY,
    owner_principal_id  VARCHAR(64)  NOT NULL,
    vpc_id              VARCHAR(64)  NOT NULL REFERENCES vpcs(id),
    subnet_id           VARCHAR(64)  NOT NULL REFERENCES subnets(id),
    elastic_ip_id       VARCHAR(64)  NOT NULL REFERENCES elastic_ips(id),
    status              VARCHAR(20)  NOT NULL DEFAULT 'pending',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,

    CONSTRAINT nat_gateways_status_check
        CHECK (status IN ('pending', 'available', 'deleting', 'deleted', 'failed'))
);

-- One NAT Gateway per subnet: a subnet cannot have two active NAT Gateways.
-- Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Anti-Loop".
CREATE UNIQUE INDEX IF NOT EXISTS idx_nat_gateways_subnet_active
    ON nat_gateways (subnet_id)
    WHERE deleted_at IS NULL;

-- Fast lookup by VPC for list operations.
CREATE INDEX IF NOT EXISTS idx_nat_gateways_vpc
    ON nat_gateways (vpc_id)
    WHERE deleted_at IS NULL;

-- Fast lookup by EIP (to enforce one-EIP-per-NAT-GW and for disassociation).
CREATE UNIQUE INDEX IF NOT EXISTS idx_nat_gateways_eip_active
    ON nat_gateways (elastic_ip_id)
    WHERE deleted_at IS NULL;

-- Owner-scoped listing.
CREATE INDEX IF NOT EXISTS idx_nat_gateways_owner
    ON nat_gateways (owner_principal_id)
    WHERE deleted_at IS NULL;
