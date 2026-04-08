# AUTH_OWNERSHIP_MODEL_V1

## Authentication, Ownership, and Access Boundary — Phase 1 Contract

**Status:** FROZEN — changes require formal review with downstream dependency analysis.

---

## 1. Authentication Mechanism

**Model:** Signed API requests using HMAC-SHA256 (AWS SigV4-style).

Users are issued an `AccessKeyId` (public identifier) and a `SecretAccessKey` (private, high-entropy string). The `SecretAccessKey` is never transmitted over the wire.

**Why this model:**
- Per-request validity and immediate revocation via key deactivation.
- JWTs rejected: difficult to revoke immediately.
- OAuth 2.0 rejected: addresses delegated third-party authorization, too complex for Phase 1 first-party machine-to-machine use.

### Request Signing Flow

```
Client:
  1. Construct canonical request string (method, URI, sorted headers, body hash)
  2. Derive signing key from SecretAccessKey + date
  3. Compute HMAC-SHA256 signature over canonical request
  4. Attach to request: Authorization header with AccessKeyId + signature

Server:
  1. Extract AccessKeyId from Authorization header
  2. Look up AccessKeyId in api_keys table
  3. Check api_keys.status = 'ACTIVE' → reject if INACTIVE
  4. Retrieve encrypted SecretAccessKey
  5. Decrypt via KMS
  6. Re-construct canonical request from received HTTP request
  7. Re-compute HMAC-SHA256 signature
  8. Compare computed vs received signature → reject if mismatch
  9. Validate timestamp: reject if request is > 5 minutes old (replay prevention)
  10. Attach principal_id to request context
```

### Request Timestamp Tolerance

Strict 5-minute window. Requests with timestamps older than 5 minutes are rejected with `401 expired_signature`. Clients must maintain accurate system clocks.

---

## 2. Key Management

### Database Schema

```sql
CREATE TABLE api_keys (
    access_key_id       VARCHAR(64) PRIMARY KEY,
    principal_id        UUID NOT NULL REFERENCES principals(id),
    secret_key_encrypted BYTEA NOT NULL,         -- envelope encrypted via KMS
    secret_key_dek_encrypted BYTEA NOT NULL,     -- encrypted DEK for this key
    status              VARCHAR(20) NOT NULL DEFAULT 'ACTIVE',  -- ACTIVE, INACTIVE
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at        TIMESTAMPTZ
);

CREATE INDEX idx_api_keys_principal
    ON api_keys (principal_id);
```

### Key Lifecycle

| Operation | Behavior |
|-----------|----------|
| **Generation** | System generates `AccessKeyId` (public) and `SecretAccessKey` (private, high-entropy). `SecretAccessKey` encrypted at rest using envelope encryption: per-key DEK encrypted by KMS CMK. Plaintext `SecretAccessKey` shown to user exactly once at creation. |
| **Storage** | Only the encrypted `SecretAccessKey` is stored. Plaintext is never persisted. |
| **Revocation** | Immediate. `UPDATE api_keys SET status = 'INACTIVE' WHERE access_key_id = $key`. Auth middleware checks status on every request. |
| **Rotation** | Multiple active keys per account. User generates new key, updates clients, verifies, revokes old key. Zero-downtime rotation. |
| **KMS dependency** | KMS unavailability causes total authentication failure. System fails closed. KMS latency is a critical platform dependency with monitoring and tight SLOs. |

### Secret Handling Rules

- `SecretAccessKey` is never returned in API responses after initial issuance.
- `SecretAccessKey` never appears in any log stream.
- SSH private keys are never stored, generated, or handled by the platform.
- Raw HTTP request/response bodies are never logged.
- Process environment variables are never logged.
- Credentials and PII beyond `user_id` are never written to any log stream.

---

## 3. Ownership Model

### Schema

```sql
CREATE TABLE principals (
    id              UUID PRIMARY KEY,
    principal_type  VARCHAR(20) NOT NULL,     -- 'ACCOUNT' in Phase 1; 'PROJECT' in Phase 2
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE accounts (
    id              UUID PRIMARY KEY,
    principal_id    UUID NOT NULL UNIQUE REFERENCES principals(id),
    email           VARCHAR(255) NOT NULL UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### Instance Ownership

The `instances` table uses `owner_principal_id UUID NOT NULL REFERENCES principals(id)`.

**This is a permanent schema decision (R-12).** The instances table does NOT use a direct `user_id` or `account_id` column. This enables Phase 2 project-level ownership without schema migration.

### API Owner Field

The `owner` field in the API resource is an ARN-structured string (R-13):

```
arn:cs:compute::{account_id}:user/{user_id}
```

This format is structured for Phase 2 IAM policy engines. The ARN is derived from `owner_principal_id` at serialization time — it is not stored as a string in the database.

### Ownership Rules

| Rule | Description |
|------|-------------|
| Immutable | Owner is set at creation and never changes. |
| Non-null | Every instance must have exactly one owner. `NOT NULL` constraint. |
| Account-level in Phase 1 | Every instance is owned by the account that created it. |
| Phase 2 path | Adding `PROJECT` to `principal_type` enum. Creating `projects` table with `principal_id` FK. Re-assigning instance ownership to project principals. No schema change to `instances` table required. |

---

## 4. Access Boundary Enforcement

### Authorization Check

Enforced in centralized middleware before request handlers. Every API endpoint that operates on a specific instance performs this check:

```
1. Authenticate: resolve AccessKeyId → principal_id (from auth middleware)
2. Authorize: SELECT owner_principal_id FROM instances WHERE id = $instance_id
3. Compare: principal_id == owner_principal_id
4. If match: proceed
5. If no match: return 404 Not Found
6. If instance does not exist: return 404 Not Found
```

**Security rule:** On unauthorized access, return `404 Not Found` (never `403 Forbidden`). This prevents confirming the existence of resources the requester does not own.

### Authorization Check Order

```
Authentication → Authorization → Existence → Validation → State Guard → Idempotency
```

Auth checks always precede existence checks. A request for a non-existent resource by an unauthenticated user returns `401`, not `404`.

### Action Names

All API calls have explicit action names in their authorization context (C-11). Phase 2 IAM requires action-level permissions.

| Endpoint | Action Name |
|----------|-------------|
| `POST /v1/instances` | `compute:CreateInstance` |
| `GET /v1/instances` | `compute:ListInstances` |
| `GET /v1/instances/{id}` | `compute:DescribeInstance` |
| `PATCH /v1/instances/{id}` | `compute:UpdateInstance` |
| `DELETE /v1/instances/{id}` | `compute:DeleteInstance` |
| `POST /v1/instances/{id}:start` | `compute:StartInstance` |
| `POST /v1/instances/{id}:stop` | `compute:StopInstance` |
| `POST /v1/instances/{id}:reboot` | `compute:RebootInstance` |
| `GET /v1/instances/{id}/events` | `compute:ListInstanceEvents` |
| `GET /v1/jobs/{id}` | `compute:DescribeJob` |

Phase 1 does not enforce action-level permissions (all actions are allowed for the resource owner). The action names are recorded in the auth context for Phase 2 IAM.

---

## 5. SSH Key Handling

### Schema

```sql
CREATE TABLE ssh_public_keys (
    id              UUID PRIMARY KEY,
    principal_id    UUID NOT NULL REFERENCES principals(id),
    name            VARCHAR(255) NOT NULL,
    public_key      TEXT NOT NULL,
    fingerprint     VARCHAR(255) NOT NULL,     -- SHA256 fingerprint
    key_type        VARCHAR(50) NOT NULL,       -- ssh-ed25519, ecdsa-sha2-nistp256, etc.
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (principal_id, name)
);
```

### Allowed Key Types

| Key Type | Accepted |
|----------|----------|
| `ssh-ed25519` | Yes |
| `ecdsa-sha2-nistp256` | Yes |
| `ecdsa-sha2-nistp384` | Yes |
| `ecdsa-sha2-nistp521` | Yes |
| `ssh-rsa` | **No — rejected in Phase 1** |

### Key Flow

```
1. User registers public key via POST /v1/ssh-keys → stored in ssh_public_keys table
2. At instance launch: control plane embeds public key in instance metadata document
3. Metadata service at 169.254.169.254 serves the key (per-host, source IP identifies instance)
4. cloud-init on first boot: fetches public key, writes to ~/.ssh/authorized_keys
5. Instance is SSH-accessible once cloud-init completes
```

### Security Rules

| Rule | Description |
|------|-------------|
| Only public key stored | Platform never stores, generates, or handles private SSH keys. |
| Fingerprint stored | SHA256 fingerprint computed and stored for display purposes. |
| Audit logging | All `SshKey.Create`, `SshKey.Delete`, and `Instance.LaunchWithKey` events logged with fingerprint (not full key text), user_id, and source IP. |
| DB RBAC | Write access to `ssh_public_keys` restricted to key management service only. |

---

## 6. Trust Boundaries

| Boundary | Mechanism |
|----------|-----------|
| **External** (client → API Server) | SigV4-signed requests. All traffic authenticated before processing. |
| **Internal** (worker → control plane) | mTLS with host-specific certificates (CN=`host-{host_id}`). Bootstrap: one-time token → CSR → signed mTLS cert. Token invalidated after first use. |
| **Metadata service** (VM → metadata) | IMDSv2 token-based. `PUT` to obtain session token, then token in header for `GET`. Source IP filtering on host ensures VMs access only their own metadata. iptables rules enforced by Host Agent. |

---

## 7. Internal Service Authentication

All inter-service communication is authenticated from day one (R-02). No unauthenticated internal endpoints at any milestone.

| Communication Path | Auth Mechanism |
|-------------------|----------------|
| API Server → Database | Connection-level auth (PostgreSQL role + TLS) |
| API Server → Message Queue | Connection-level auth |
| Worker → Control Plane APIs | mTLS (host certificate) |
| Host Agent → Resource Manager | mTLS (host certificate) |
| Reconciler → Database | Connection-level auth (service account) |
| Reconciler → Message Queue | Connection-level auth (service account) |

---

## 8. Open Implementation Decisions

| ID | Question | Impact |
|----|----------|--------|
| OID-AU-1 | Exact canonical request specification for SigV4 signing: full AWS SigV4 or simplified subset? | SDK and CLI development cannot begin until signing spec is locked. |
| OID-AU-2 | How does the web console authenticate? OIDC + session cookie? What internal token does the console backend use for API calls? | Two separate auth paths create a security model gap. |
| OID-AU-3 | Per-account default quotas: max instances, max vCPUs, max public IPs? | Without quota enforcement, a single tenant can exhaust the fleet. |
| OID-AU-4 | Data retention policy for deleted instance records and associated events? | Unbounded storage growth without a purge policy. |
