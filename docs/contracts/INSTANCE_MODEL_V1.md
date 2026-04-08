# INSTANCE_MODEL_V1

## Canonical Instance Domain Model — Phase 1 Contract

**Status:** FROZEN — changes require formal review with downstream dependency analysis.

---

## 1. Instance Identity

Every instance has a dual identity:

- **Canonical ID (`id`):** System-generated, globally unique, immutable opaque string. Format: `inst_` prefix + KSUID (e.g., `inst_2nMpMz5Ge4VYeRBpaFKsx6Y7Fkn`). This is the authoritative primary key in the database, all event payloads, logs, and internal service communication. Never reused after deletion.
- **Display Name (`name`):** User-provided, mutable, human-readable string. Unique within `(owner_principal_id, region)` scope. The API resolves names to canonical IDs for all state-changing operations. The control plane always operates on the canonical ID internally.

**ID generation rules:**
- KSUID or equivalent time-sortable, globally unique generator.
- Auto-incrementing integers are never used as user-visible IDs.
- ID is assigned at creation and never changes.
- IDs are never recycled after instance deletion.

---

## 2. Instance Object Model (API Resource)

The following defines the canonical JSON shape returned by `GET /v1/instances/{id}` and embedded in `GET /v1/instances` list responses.

| Field | Type | Mutability | Population | Nullable | Description |
|-------|------|------------|------------|----------|-------------|
| `id` | `string` | Immutable | Sync (System) | No | Canonical PK. `inst_` prefix + KSUID. |
| `name` | `string` | Mutable | Sync (User) | No | Human-readable alias. Unique within `(owner, region)`. Max 63 chars. `^[a-z][a-z0-9-]{0,61}[a-z0-9]$`. |
| `owner` | `string` (ARN) | Immutable | Sync (System) | No | `arn:cs:compute::{account_id}:user/{user_id}`. |
| `image_id` | `string` | Immutable | Sync (User) | No | ID of the machine image used to boot. `img_` prefix. |
| `instance_type` | `string` | Immutable | Sync (User) | No | Shape identifier (e.g., `gp1.large`). |
| `status` | `string` (enum) | System-managed | Async | No | Lifecycle state. One of the 9 canonical states. |
| `region` | `string` | Immutable | Sync (System) | No | Derived from API endpoint. |
| `availability_zone` | `string` | Immutable | Sync (User) | No | Failure domain within region. |
| `host` | `string` | System-managed | Async | Yes | Physical hypervisor hostname. Null until scheduled. Not exposed in Phase 1 API responses. |
| `private_ip` | `string` (IPv4) | System-managed | Async | Yes | Null until DHCP allocation. Host-local in Phase 1. |
| `public_ip` | `string` (IPv4) | System-managed | Async | Yes | Null unless requested. 1:1 NAT. |
| `ssh_key_name` | `string` | Immutable | Sync (User) | No | References pre-registered public key by name. |
| `created_at` | `string` (ISO 8601) | Immutable | Sync (System) | No | UTC, millisecond precision. |
| `updated_at` | `string` (ISO 8601) | System-managed | Sync/Async | No | Updated on every state change. Used for optimistic locking. |
| `labels` | `object` (map<string,string>) | Mutable | Sync (User) | No | User-defined key-value metadata. Empty object `{}` if none. Max 64 keys. Key max 63 chars, value max 255 chars. |
| `status_details` | `object` | System-managed | Async | Yes | Machine-readable error context on failure states. Null when not in `failed` state. |
| `block_devices` | `array` | Immutable | Sync (User) | No | Block device mapping. Phase 1: exactly one entry. |

**`status_details` shape (when present):**

```json
{
  "code": "PROVISION_TIMEOUT",
  "message": "Instance provisioning timed out after 15 minutes."
}
```

**`block_devices` item shape:**

```json
{
  "image_id": "img_...",
  "size_gb": 160,
  "delete_on_termination": true
}
```

Phase 1 constraint: `delete_on_termination` must be `true`. Setting it to `false` returns a `400` validation error. The field exists in the API from day one for Phase 2 forward-compatibility.

---

## 3. Immutability Contract

The following fields are locked at creation. Changing any of them is semantically equivalent to creating a new instance:

`id`, `owner`, `region`, `availability_zone`, `image_id`, `instance_type`, `ssh_key_name`, `created_at`, `block_devices`

**Mutable fields (via `PATCH /v1/instances/{id}`):**

`name`, `labels`

PATCH must never be used for lifecycle state transitions. State transitions use `POST /v1/instances/{id}:{action}`.

---

## 4. Database Table: `instances`

```sql
CREATE TABLE instances (
    id                  UUID PRIMARY KEY,
    owner_principal_id  UUID NOT NULL REFERENCES principals(id),
    display_name        VARCHAR(63),
    vm_state            VARCHAR(50) NOT NULL,          -- lifecycle state enum
    task_state          VARCHAR(50),                    -- current async operation, nullable
    image_ref           VARCHAR(255) NOT NULL,
    instance_type_id    VARCHAR(255) NOT NULL,
    vcpus               INTEGER NOT NULL,
    memory_mb           INTEGER NOT NULL,
    root_gb             INTEGER NOT NULL,
    host                VARCHAR(255),                   -- nullable until scheduled
    availability_zone   VARCHAR(255) NOT NULL,
    spec                JSONB NOT NULL,                 -- desired config (ssh_key, labels, block_devices, etc.)
    actual_spec         JSONB,                          -- reported runtime config from host agent
    locked_by           UUID,                           -- job_id holding exclusive mutation lock
    version             INTEGER NOT NULL DEFAULT 0,     -- optimistic locking
    launched_at         TIMESTAMPTZ,
    terminated_at       TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ                     -- soft-delete marker
);

-- Uniqueness: display_name is unique per owner+region (enforced at application layer or partial unique index)
CREATE UNIQUE INDEX idx_instances_owner_name
    ON instances (owner_principal_id, display_name)
    WHERE deleted_at IS NULL;
```

**Key differences between DB columns and API fields:**
- DB uses `owner_principal_id` (UUID FK); API returns `owner` (ARN string).
- DB uses `display_name`; API uses `name`.
- DB uses `vm_state` and `task_state`; API returns a single `status` field derived from these.
- DB uses `image_ref`; API uses `image_id`.
- DB stores `vcpus`, `memory_mb`, `root_gb` denormalized from instance type for scheduler and cgroup enforcement.
- DB `spec` JSONB contains `ssh_key_name`, `labels`, `block_devices`, `user_data`, etc.

---

## 5. Indexes

| Index | Columns | Purpose |
|-------|---------|---------|
| PK | `(id)` | Primary key lookup |
| `idx_instances_owner_created` | `(owner_principal_id, created_at DESC)` | `ListInstances` for a user, sorted by creation time |
| `idx_instances_state_updated` | `(vm_state, updated_at ASC)` | Reconciler: find instances stuck in transitional state |
| `idx_instances_host_state` | `(host, vm_state)` | Reconciler: find all running instances on a specific host |
| `idx_instances_not_deleted` | `(deleted_at)` partial `WHERE deleted_at IS NULL` | Filter soft-deleted instances from normal queries |
| `idx_instances_owner_name` | `(owner_principal_id, display_name)` partial `WHERE deleted_at IS NULL` | Name uniqueness within owner scope |

---

## 6. Instance Shapes (Phase 1 Catalog)

| Shape | vCPU | RAM (GB) | Root Disk (GB) |
|-------|------|----------|----------------|
| `gp1.small` | 2 | 4 | 50 |
| `gp1.medium` | 2 | 8 | 80 |
| `gp1.large` | 4 | 16 | 160 |
| `gp1.xlarge` | 8 | 32 | 320 |

Naming convention: `{family}{generation}.{size}`. Shape is immutable on a live instance. The generation number (`1`) namespaces future hardware refreshes (`gp2.small`) without API breakage.

---

## 7. Image Model (Phase 1)

Phase 1 offers a curated set of platform-provided base images. All images must include cloud-init.

```sql
CREATE TABLE images (
    id              UUID PRIMARY KEY,
    name            VARCHAR(255) NOT NULL,
    os_family       VARCHAR(50) NOT NULL,       -- e.g., 'ubuntu', 'debian'
    os_version      VARCHAR(50) NOT NULL,       -- e.g., '22.04', '12'
    architecture    VARCHAR(20) NOT NULL DEFAULT 'x86_64',
    owner_id        UUID NOT NULL,              -- system account in Phase 1
    visibility      VARCHAR(20) NOT NULL DEFAULT 'PUBLIC',  -- Phase 2: 'PRIVATE'
    source_type     VARCHAR(20) NOT NULL DEFAULT 'PLATFORM', -- Phase 2: 'USER'
    storage_url     VARCHAR(1024) NOT NULL,     -- object store URL for base image
    min_disk_gb     INTEGER NOT NULL,
    status          VARCHAR(20) NOT NULL DEFAULT 'ACTIVE',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

Phase 2 readiness: `owner_id`, `visibility`, and `source_type` fields exist from day one.

---

## 8. Root Disk Model

The root disk is a first-class internal object stored independently from the instance.

```sql
CREATE TABLE root_disks (
    disk_id                 UUID PRIMARY KEY,
    instance_id             UUID REFERENCES instances(id),  -- NULL if detached
    source_image_id         UUID NOT NULL REFERENCES images(id),
    storage_pool_id         UUID NOT NULL,
    storage_path            VARCHAR(1024) NOT NULL,
    size_gb                 INTEGER NOT NULL,
    delete_on_termination   BOOLEAN NOT NULL DEFAULT TRUE,
    status                  VARCHAR(32) NOT NULL,           -- CREATING, ATTACHED, DETACHED
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**Status values:**
- `CREATING` — CoW overlay being materialized.
- `ATTACHED` — Bound to a running or stopped instance.
- `DETACHED` — Instance deleted with `delete_on_termination=false`. Phase 2 entry point for persistent volumes.

**Storage model:** Network-attached NFS. qcow2 CoW overlays on shared NFS export. All hypervisors mount the same export.

**Persistence contract:**
- stop/start: root disk data preserved. Instance may land on different host.
- reboot: root disk data preserved. Same host.
- delete (default): root disk destroyed. Irreversible.

---

## 9. Invariants

| ID | Invariant | Enforcement |
|----|-----------|-------------|
| I-1 | Instance IDs are globally unique and never reused. | `PRIMARY KEY` + KSUID generator. |
| I-3 | A root disk may only be attached read-write to one instance at a time. | `UNIQUE(disk_id)` on read-write attachments + worker precondition check. |
| I-4 | An instance in a given state must have exactly the resource set implied by that state. | State transition handlers + reconciler verification. |
| I-6 | Every instance has exactly one, non-null, immutable owner. | `NOT NULL` + `REFERENCES principals(id)` on `owner_principal_id`. |
| I-7 | At most one state-mutating job is active for a given instance at a time. | `locked_by` field + atomic claim; 409 on conflict. |

---

## 10. Open Implementation Decisions

| ID | Question | Impact |
|----|----------|--------|
| OID-IM-1 | Exact format of the `inst_` prefix ID: KSUID, ULID, or Snowflake? All satisfy requirements. | ID generator implementation. |
| OID-IM-2 | Should `host` be exposed in the Phase 1 API response? Master Blueprint lists it as a field but it exposes infrastructure topology. | API serialization. |
| OID-IM-3 | Exact validation regex and length constraints for `labels` keys and values? | API validation middleware. |
