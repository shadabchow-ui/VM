-- 003_cross_host_networking.up.sql — M9 Slice 4: Cross-host networking groundwork.
--
-- Source: P2_VPC_NETWORK_CONTRACT §8.2 (VTEP management),
--         PHASE_2_MASTER_PLAN §8 (networking subsystem evolution),
--         P2_MILESTONE_PLAN §P2-M2, §P2-M3 (VPC foundation, cross-host networking).
--
-- This migration adds the control-plane foundation for future cross-host private
-- networking (VXLAN overlay). It does NOT implement the actual dataplane.
--
-- Tables added:
--   1. host_tunnel_endpoints — per-host VTEP identity and underlay IP
--   2. vpc_host_attachments — tracks which hosts participate in which VPCs
--
-- These tables enable the network controller to:
--   - Know each host's VTEP underlay IP for tunnel endpoint routing
--   - Track which hosts have instances in each VPC (for VTEP propagation)
--   - Assign and track VXLAN VNIs per VPC

-- ── 1. host_tunnel_endpoints ─────────────────────────────────────────────────
-- Per-host VTEP identity. Each registered host reports its VTEP underlay IP
-- during registration. The network controller uses this to build the VTEP
-- routing table for cross-host traffic.
--
-- Source: P2_VPC_NETWORK_CONTRACT §8.2 (each compute host has one VTEP interface).
CREATE TABLE host_tunnel_endpoints (
    host_id             VARCHAR(64)  NOT NULL PRIMARY KEY,  -- matches hosts.id / AGENT_HOST_ID
    vtep_ip             INET         NOT NULL,              -- underlay IP for VXLAN tunnel endpoint
    vtep_mac            MACADDR,                            -- optional: VTEP interface MAC
    vtep_interface      VARCHAR(64)  NOT NULL DEFAULT 'vxlan0',  -- VTEP interface name
    status              VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT hte_status_check CHECK (
        status IN ('active', 'draining', 'offline')
    )
);

-- Index for looking up active VTEPs
CREATE INDEX idx_hte_status
    ON host_tunnel_endpoints (status)
    WHERE status = 'active';

-- ── 2. vpc_host_attachments ──────────────────────────────────────────────────
-- Tracks which hosts participate in which VPCs. When an instance is created
-- in a VPC on a host, an attachment record is created (or ref count incremented).
-- The network controller uses this to know which hosts need VTEP entries for
-- a given VPC.
--
-- Source: P2_VPC_NETWORK_CONTRACT §8.2 (network controller propagates VTEP entries
--         to all other hosts in the VPC).
CREATE TABLE vpc_host_attachments (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- vha_ + KSUID
    vpc_id              VARCHAR(64)  NOT NULL REFERENCES vpcs(id),
    host_id             VARCHAR(64)  NOT NULL,  -- references host_tunnel_endpoints(host_id)
    instance_count      INTEGER      NOT NULL DEFAULT 1,    -- number of VPC instances on this host
    first_attached_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT vha_instance_count_positive CHECK (instance_count >= 0),
    UNIQUE (vpc_id, host_id)
);

-- Index for finding all hosts in a VPC (for VTEP propagation)
CREATE INDEX idx_vha_vpc_id
    ON vpc_host_attachments (vpc_id)
    WHERE instance_count > 0;

-- Index for finding all VPCs on a host (for host draining/removal)
CREATE INDEX idx_vha_host_id
    ON vpc_host_attachments (host_id)
    WHERE instance_count > 0;

-- ── 3. vni_allocations ───────────────────────────────────────────────────────
-- Tracks VXLAN Network Identifier (VNI) allocations. Each VPC is assigned a
-- unique VNI system-wide. VNIs are allocated from a pool and never reused
-- (to avoid stale VTEP entries causing cross-VPC leakage).
--
-- Source: P2_VPC_NETWORK_CONTRACT §8.1 (each VPC is assigned a unique VNI system-wide).
--
-- Note: The vpcs.vxlan_vni column (already in 002) stores the assigned VNI.
-- This table provides atomic allocation via SELECT FOR UPDATE SKIP LOCKED.
CREATE TABLE vni_allocations (
    vni                 INTEGER      NOT NULL PRIMARY KEY,  -- VXLAN VNI (4096-16777215 valid range)
    allocated           BOOLEAN      NOT NULL DEFAULT FALSE,
    vpc_id              VARCHAR(64)  REFERENCES vpcs(id),   -- NULL if not allocated
    allocated_at        TIMESTAMPTZ,
    CONSTRAINT vni_range_check CHECK (vni >= 4096 AND vni <= 16777215)
);

-- Index for finding available VNIs
CREATE INDEX idx_vni_available
    ON vni_allocations (allocated, vni)
    WHERE allocated = FALSE;

-- Seed VNI pool: 4096-8191 (4096 VNIs for Phase 2, expandable later)
-- Source: VXLAN spec allows 4096-16777215; we start with a small pool.
INSERT INTO vni_allocations (vni, allocated)
SELECT g, FALSE
FROM generate_series(4096, 8191) g
ON CONFLICT DO NOTHING;

-- ── 4. nic_vtep_registrations ────────────────────────────────────────────────
-- Tracks NIC → host VTEP registrations. When a VPC instance is created, the
-- host agent registers the NIC's MAC and IP with the network controller.
-- This enables the network controller to build the forwarding table:
--   (vpc_id, instance_private_ip) → host_VTEP_IP
--
-- Source: P2_VPC_NETWORK_CONTRACT §8.2 (host agent registers NIC MAC/IP).
CREATE TABLE nic_vtep_registrations (
    id                  VARCHAR(64)  NOT NULL PRIMARY KEY,  -- nvr_ + KSUID
    nic_id              VARCHAR(64)  NOT NULL REFERENCES network_interfaces(id) ON DELETE CASCADE,
    vpc_id              VARCHAR(64)  NOT NULL REFERENCES vpcs(id),
    host_id             VARCHAR(64)  NOT NULL,  -- references host_tunnel_endpoints(host_id)
    private_ip          INET         NOT NULL,
    mac_address         MACADDR      NOT NULL,
    vni                 INTEGER      NOT NULL,              -- VPC's assigned VNI
    status              VARCHAR(20)  NOT NULL DEFAULT 'active',
    registered_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT nvr_status_check CHECK (
        status IN ('pending', 'active', 'stale', 'removed')
    )
);

-- Index for looking up all registrations in a VPC (for VTEP table building)
CREATE INDEX idx_nvr_vpc_id
    ON nic_vtep_registrations (vpc_id)
    WHERE status = 'active';

-- Index for looking up all registrations on a host (for host draining)
CREATE INDEX idx_nvr_host_id
    ON nic_vtep_registrations (host_id)
    WHERE status = 'active';

-- Index for looking up by private IP within VPC (for forwarding table lookup)
CREATE INDEX idx_nvr_vpc_ip
    ON nic_vtep_registrations (vpc_id, private_ip)
    WHERE status = 'active';
