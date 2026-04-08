# IP_ALLOCATION_CONTRACT_V1

## IP Allocation and Network Interface — Phase 1 Contract

**Status:** FROZEN — changes require formal review with downstream dependency analysis.

---

## 1. Phase 1 Network Model

Host-local Linux bridge (`br0`) with NAT. Each compute host manages a private `10.0.0.0/8` subnet for its instances. All instance TAP devices connect to the host bridge. Outbound internet traffic uses host-level `iptables MASQUERADE` (source NAT).

**Phase 1 limitations:**
- Cross-host private networking is NOT supported. Two instances on different hosts cannot reach each other via private IP.
- Per-instance bandwidth shaping is NOT enforced. A single instance can saturate the host NIC uplink.
- The networking model references instances as attached to a network object (a platform-managed default), not directly to a host bridge (R-20).

---

## 2. Database Schema

```sql
CREATE TABLE ip_allocations (
    ip_address          INET NOT NULL,
    vpc_id              UUID NOT NULL,            -- platform-managed default VPC in Phase 1
    allocated           BOOLEAN NOT NULL DEFAULT FALSE,
    owner_interface_id  UUID,                     -- vNIC ID, nullable when unallocated
    instance_id         UUID REFERENCES instances(id),  -- nullable when unallocated
    allocated_at        TIMESTAMPTZ,
    released_at         TIMESTAMPTZ,
    PRIMARY KEY (ip_address, vpc_id),
    UNIQUE (vpc_id, ip_address)
);

CREATE INDEX idx_ip_available
    ON ip_allocations (vpc_id, allocated) WHERE allocated = FALSE;

CREATE INDEX idx_ip_by_instance
    ON ip_allocations (instance_id) WHERE instance_id IS NOT NULL;
```

**Phase 1:** A single platform-managed `vpc_id` is used for all instances. The `vpc_id` column exists from day one to enable Phase 2 VPC introduction without schema changes.

---

## 3. IP Pool Initialization

The IP pool is pre-seeded with available addresses:

```sql
-- Example: seed a /24 private range
INSERT INTO ip_allocations (ip_address, vpc_id, allocated)
SELECT host(ip)::inet, $default_vpc_id, FALSE
FROM generate_series(1, 254) AS ip
WHERE host(('10.0.1.' || ip)::inet) IS NOT NULL;
```

Public IP pool is similarly pre-seeded from a managed allocation.

---

## 4. Allocation Transaction (Critical Path)

IP allocation is a safety-critical operation. Invariant I-2: No two network interfaces share the same IP within a VPC.

### Atomic Allocation

```sql
BEGIN;

-- Select and lock an available IP
SELECT ip_address FROM ip_allocations
WHERE vpc_id = $vpc_id
  AND allocated = FALSE
ORDER BY ip_address ASC
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- If no rows returned: IP pool exhausted → fail provisioning

-- Assign to instance
UPDATE ip_allocations
SET allocated = TRUE,
    owner_interface_id = $vnic_id,
    instance_id = $instance_id,
    allocated_at = NOW(),
    released_at = NULL
WHERE ip_address = $selected_ip
  AND vpc_id = $vpc_id
  AND allocated = FALSE;

COMMIT;
```

**`FOR UPDATE SKIP LOCKED`** ensures concurrent allocation requests do not deadlock — each worker locks a different row.

### Idempotent Re-allocation

If an IP is already allocated to the same `instance_id`, the operation returns the existing allocation without modification:

```sql
SELECT ip_address FROM ip_allocations
WHERE instance_id = $instance_id
  AND vpc_id = $vpc_id
  AND allocated = TRUE;

-- If found: return existing IP, do not allocate a new one.
```

---

## 5. Release Transaction

IP release is idempotent. Releasing an already-released IP is a no-op.

```sql
UPDATE ip_allocations
SET allocated = FALSE,
    owner_interface_id = NULL,
    instance_id = NULL,
    released_at = NOW()
WHERE instance_id = $instance_id
  AND vpc_id = $vpc_id;

-- Rows affected: 0 = already released (idempotent no-op), 1+ = released successfully.
```

### When IPs Are Released

| Instance Transition | Private IP | Public IP |
|--------------------|------------|-----------|
| `RUNNING → STOPPING → STOPPED` | Retained (stable for stop/start) | Released. New public IP on restart. |
| `* → DELETING → DELETED` | Released | Released |
| `* → FAILED` (from provisioning) | Released (rollback) | Released (rollback) |

**Private IP stability:** Private IPs are stable for the life of a running or stopped instance. They are only released on deletion or provisioning failure.

**Public IP volatility:** Public IPs are released on stop and re-allocated on start. Users should not depend on public IP persistence across stop/start cycles.

---

## 6. Public IP — NAT Rules

Public IPs are implemented via 1:1 NAT on the host:

```bash
# DNAT: inbound traffic to public IP → private IP
iptables -t nat -A PREROUTING -d $public_ip -j DNAT --to-destination $private_ip

# SNAT: outbound traffic from private IP → public IP
iptables -t nat -A POSTROUTING -s $private_ip -j SNAT --to-source $public_ip
```

**Default firewall:** Port 22 (SSH) open by default. All other ports `DROP` unless explicitly opened via `AddFirewallRule`.

**Rule lifecycle:**
- Rules programmed by Host Agent during provisioning (step 7 of CreateInstance).
- Rules removed on stop (Host Agent StopInstance sequence) and delete.
- Rules re-programmed on start (new public IP, potentially new host).

---

## 7. Virtual Network Interface (VIF) Lifecycle

Each instance has one vNIC in Phase 1. The vNIC is represented by a TAP device on the host.

| Operation | Timing | Host Agent Action |
|-----------|--------|-------------------|
| Create TAP | During provisioning, after IP allocation | `ip tuntap add dev tap-{instance_short_id} mode tap` |
| Attach to bridge | During provisioning | `ip link set tap-{...} master br0` |
| Bring up | During provisioning | `ip link set tap-{...} up` |
| Remove on stop | During StopInstance | Remove from bridge, leave device for potential restart |
| Delete on delete | During DeleteInstance | `ip tuntap del dev tap-{...} mode tap` |

**MAC address:** Generated deterministically from `instance_id` to ensure uniqueness. Format: `02:xx:xx:xx:xx:xx` (locally administered bit set).

---

## 8. DHCP

**Private IP assignment:** DHCP relay on each host forwards `DHCPDISCOVER` to a centralized, HA DHCP cluster. Leases are long-lived (7 days) with static reservations tied to the instance's virtual MAC address.

Private IPs are stable for the life of a running or stopped instance.

---

## 9. IP Uniqueness Reconciler Sub-Scan

The reconciler runs a periodic sub-scan to detect invariant violations:

```sql
SELECT ip_address, vpc_id, COUNT(*) AS allocation_count
FROM ip_allocations
WHERE allocated = TRUE
GROUP BY ip_address, vpc_id
HAVING COUNT(*) > 1;
```

**If any rows returned:**
- Alert: invariant I-2 violation detected.
- Fence the most recently allocated interface (transition instance to `ERROR_QUARANTINED`).
- No automated recovery. Operator investigation required.

---

## 10. Invariants

| ID | Invariant | Enforcement |
|----|-----------|-------------|
| I-2 | No two network interfaces share the same IP within a VPC. | `UNIQUE(vpc_id, ip_address)` constraint + `SELECT FOR UPDATE SKIP LOCKED` on allocation. |
| — | IP released on instance deletion. | DeleteInstance handler releases IP as a compensating action. Verified by post-deletion inventory check. |
| — | IP released on provisioning failure rollback. | Rollback releases IP in reverse order. Idempotent: releasing an unallocated IP is a no-op. |
| — | No IP pool exhaustion without alert. | Alert when pool utilization > 80%. |
| R-20 | Networking model references a network object, not a host bridge. | `vpc_id` column on `ip_allocations`. Phase 1 uses platform-managed default. |

---

## 11. Open Implementation Decisions

| ID | Question | Impact |
|----|----------|--------|
| OID-IP-1 | Exact private IP range per host: `/24` from `10.0.0.0/8`, or per-host allocation from a central IPAM? | Host Agent network configuration and DHCP setup. |
| OID-IP-2 | Public IP pool sizing for Phase 1 launch: how many public IPs to pre-allocate? | Capacity planning and alert thresholds. |
| OID-IP-3 | Should private IP be retained on stop/start (as stated in Master Blueprint) or re-allocated? Master Blueprint says stable; Execution Blueprint's stop flow says "retain IP, retain rootfs." Confirmed: retain on stop. | Worker StopInstance implementation. |
| OID-IP-4 | Does Phase 1 metadata service run on a link-local address per host, or a centralized service with source-IP routing? (Open Question OQ-14 from Master Blueprint) | Host Agent network plumbing architecture. |
