-- 002_vpc_networking.up.sql — Phase 2 VPC networking schema.
--
-- Source: P2_VPC_NETWORK_CONTRACT §2.5 (vpcs), §3.3 (subnets), §4.6 (security_groups),
--         §5.4 (network_interfaces), P2_MILESTONE_PLAN §P2-M2 (VPC Foundation).
--
-- M9 Slice 1: VPC, Subnet, RouteTable control-plane foundation.
-- Table creation order respects FK dependencies.

-- ── 1. vpcs ──────────────────────────────────────────────────────────────────
-- Source: P2_VPC_NETWORK_CONTRACT §2.5.
CREATE TABLE vpcs (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- vpc_ + KSUID
    owner_principal_id  UUID         NOT NULL REFERENCES principals(id),
    name                VARCHAR(63)  NOT NULL,
    region              VARCHAR(64)  NOT NULL,
    cidr_ipv4           CIDR         NOT NULL,
    status              VARCHAR(20)  NOT NULL DEFAULT 'pending',
    vxlan_vni           INTEGER,  -- System-assigned; NULL until provisioning completes
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,
    CONSTRAINT vpcs_status_check CHECK (
        status IN ('pending', 'active', 'deleting', 'deleted')
    )
);

-- Unique name per owner (soft-delete aware)
CREATE UNIQUE INDEX idx_vpcs_owner_name
    ON vpcs (owner_principal_id, name)
    WHERE deleted_at IS NULL;

-- Prevent overlapping CIDRs for same owner (soft-delete aware)
-- Note: Overlap check is done at application layer; this prevents exact duplicates.
CREATE UNIQUE INDEX idx_vpcs_owner_cidr
    ON vpcs (owner_principal_id, cidr_ipv4)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_vpcs_owner_created
    ON vpcs (owner_principal_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- ── 2. subnets ───────────────────────────────────────────────────────────────
-- Source: P2_VPC_NETWORK_CONTRACT §3.3.
CREATE TABLE subnets (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- subnet_ + KSUID
    vpc_id              VARCHAR(64)  NOT NULL REFERENCES vpcs(id),
    name                VARCHAR(63)  NOT NULL,
    cidr_ipv4           CIDR         NOT NULL,
    availability_zone   VARCHAR(64)  NOT NULL,
    status              VARCHAR(20)  NOT NULL DEFAULT 'pending',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,
    CONSTRAINT subnets_status_check CHECK (
        status IN ('pending', 'active', 'deleting', 'deleted')
    )
);

-- Unique name per VPC (soft-delete aware)
CREATE UNIQUE INDEX idx_subnets_vpc_name
    ON subnets (vpc_id, name)
    WHERE deleted_at IS NULL;

-- Unique CIDR per VPC (soft-delete aware)
CREATE UNIQUE INDEX idx_subnets_vpc_cidr
    ON subnets (vpc_id, cidr_ipv4)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_subnets_vpc_created
    ON subnets (vpc_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- ── 3. route_tables ──────────────────────────────────────────────────────────
-- Source: P2_VPC_NETWORK_CONTRACT §11 P2-VPC-OD-3 (implicit default route table).
-- Phase 2 uses implicit single-default route table per VPC.
-- Schema is present for future explicit route table support.
CREATE TABLE route_tables (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- rtb_ + KSUID
    vpc_id              VARCHAR(64)  NOT NULL REFERENCES vpcs(id),
    name                VARCHAR(63)  NOT NULL,
    is_default          BOOLEAN      NOT NULL DEFAULT FALSE,
    status              VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,
    CONSTRAINT route_tables_status_check CHECK (
        status IN ('pending', 'active', 'deleting', 'deleted')
    )
);

-- Only one default route table per VPC
CREATE UNIQUE INDEX idx_route_tables_vpc_default
    ON route_tables (vpc_id)
    WHERE is_default = TRUE AND deleted_at IS NULL;

CREATE INDEX idx_route_tables_vpc_created
    ON route_tables (vpc_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- ── 4. route_entries ─────────────────────────────────────────────────────────
-- Minimal route entry model. Phase 2 uses implicit local + IGW routes.
CREATE TABLE route_entries (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- rte_ + KSUID
    route_table_id      VARCHAR(64)  NOT NULL REFERENCES route_tables(id) ON DELETE CASCADE,
    destination_cidr    CIDR         NOT NULL,
    target_type         VARCHAR(20)  NOT NULL,  -- 'local', 'igw', 'nat', 'peering'
    target_id           VARCHAR(64),            -- igw_xxx, nat_xxx, etc. NULL for 'local'
    priority            INTEGER      NOT NULL DEFAULT 100,
    status              VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT route_entries_target_type_check CHECK (
        target_type IN ('local', 'igw', 'nat', 'peering')
    ),
    CONSTRAINT route_entries_status_check CHECK (
        status IN ('pending', 'active', 'blackhole')
    )
);

CREATE INDEX idx_route_entries_route_table
    ON route_entries (route_table_id, priority ASC);

-- ── 5. security_groups ───────────────────────────────────────────────────────
-- Source: P2_VPC_NETWORK_CONTRACT §4.6.
CREATE TABLE security_groups (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- sg_ + KSUID
    vpc_id              VARCHAR(64)  NOT NULL REFERENCES vpcs(id),
    owner_principal_id  UUID         NOT NULL REFERENCES principals(id),
    name                VARCHAR(63)  NOT NULL,
    description         TEXT,
    is_default          BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ
);

-- Unique name per VPC (soft-delete aware)
CREATE UNIQUE INDEX idx_security_groups_vpc_name
    ON security_groups (vpc_id, name)
    WHERE deleted_at IS NULL;

-- Only one default security group per VPC
CREATE UNIQUE INDEX idx_security_groups_vpc_default
    ON security_groups (vpc_id)
    WHERE is_default = TRUE AND deleted_at IS NULL;

CREATE INDEX idx_security_groups_vpc_created
    ON security_groups (vpc_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- ── 6. security_group_rules ──────────────────────────────────────────────────
-- Source: P2_VPC_NETWORK_CONTRACT §4.6.
CREATE TABLE security_group_rules (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- sgr_ + KSUID
    security_group_id   VARCHAR(64)  NOT NULL REFERENCES security_groups(id) ON DELETE CASCADE,
    direction           VARCHAR(10)  NOT NULL,  -- 'ingress' | 'egress'
    protocol            VARCHAR(10)  NOT NULL,  -- 'tcp', 'udp', 'icmp', 'all'
    port_from           INTEGER,
    port_to             INTEGER,
    cidr                CIDR,
    source_sg_id        VARCHAR(64)  REFERENCES security_groups(id),
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT sgr_direction_check CHECK (direction IN ('ingress', 'egress')),
    CONSTRAINT sgr_protocol_check CHECK (protocol IN ('tcp', 'udp', 'icmp', 'all')),
    CONSTRAINT sgr_source_xor CHECK (
        -- SG-I-5: Cannot specify both cidr and source_sg_id
        (cidr IS NULL AND source_sg_id IS NOT NULL) OR
        (cidr IS NOT NULL AND source_sg_id IS NULL) OR
        (cidr IS NULL AND source_sg_id IS NULL)
    )
);

CREATE INDEX idx_sgr_security_group
    ON security_group_rules (security_group_id);

-- ── 7. network_interfaces ────────────────────────────────────────────────────
-- Source: P2_VPC_NETWORK_CONTRACT §5.4.
CREATE TABLE network_interfaces (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- eni_ + KSUID
    instance_id         VARCHAR(64)  NOT NULL REFERENCES instances(id),
    subnet_id           VARCHAR(64)  NOT NULL REFERENCES subnets(id),
    vpc_id              VARCHAR(64)  NOT NULL REFERENCES vpcs(id),
    private_ip          INET         NOT NULL,
    mac_address         MACADDR      NOT NULL,
    is_primary          BOOLEAN      NOT NULL DEFAULT TRUE,
    status              VARCHAR(20)  NOT NULL DEFAULT 'pending',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,
    CONSTRAINT nic_status_check CHECK (
        status IN ('pending', 'attaching', 'attached', 'detaching', 'detached')
    )
);

-- Only one primary NIC per instance
CREATE UNIQUE INDEX idx_nic_instance_primary
    ON network_interfaces (instance_id)
    WHERE is_primary = TRUE AND deleted_at IS NULL;

CREATE INDEX idx_nic_subnet
    ON network_interfaces (subnet_id)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_nic_vpc
    ON network_interfaces (vpc_id)
    WHERE deleted_at IS NULL;

-- ── 8. nic_security_groups ───────────────────────────────────────────────────
-- Source: P2_VPC_NETWORK_CONTRACT §5.4.
CREATE TABLE nic_security_groups (
    nic_id              VARCHAR(64)  NOT NULL REFERENCES network_interfaces(id) ON DELETE CASCADE,
    security_group_id   VARCHAR(64)  NOT NULL REFERENCES security_groups(id),
    PRIMARY KEY (nic_id, security_group_id)
);

-- ── 9. subnet_ip_allocations ─────────────────────────────────────────────────
-- Source: IP_ALLOCATION_CONTRACT_V1 pattern applied to VPC subnets.
-- Pre-populated when subnet is created. Uses SELECT FOR UPDATE SKIP LOCKED.
CREATE TABLE subnet_ip_allocations (
    ip_address          INET         NOT NULL,
    subnet_id           VARCHAR(64)  NOT NULL REFERENCES subnets(id),
    allocated           BOOLEAN      NOT NULL DEFAULT FALSE,
    owner_instance_id   VARCHAR(64)  REFERENCES instances(id),
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (ip_address, subnet_id)
);

CREATE INDEX idx_subnet_ip_available
    ON subnet_ip_allocations (subnet_id, allocated)
    WHERE allocated = FALSE;

-- ── 10. subnet_route_table_associations ──────────────────────────────────────
-- Associates subnets with route tables. If no association, use VPC default route table.
CREATE TABLE subnet_route_table_associations (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- srta_ + KSUID
    subnet_id           VARCHAR(64)  NOT NULL REFERENCES subnets(id) UNIQUE,
    route_table_id      VARCHAR(64)  NOT NULL REFERENCES route_tables(id),
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
