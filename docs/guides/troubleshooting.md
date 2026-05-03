# Compute Platform — Troubleshooting Guide

This guide covers diagnosis and resolution steps for common VM platform
operational issues. Each section lists symptoms, likely causes, and actions.

## 1. Failed Provisioning

**Symptom:** Instance remains in `provisioning` or `requested` state > 15 minutes,
then transitions to `failed`.

**Checkpoints:**
1. View the instance's events:
   ```bash
   curl -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$ID/events | jq .
   ```
2. Check the latest job status:
   ```bash
   curl -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$ID/jobs/$JOB_ID | jq .
   ```
3. Look for `instance.failure` events with `error_code` in metadata.

**Common causes:**

| Error Code | Meaning | Mitigation |
|---|---|---|
| `HOST_UNAVAILABLE` | No host with sufficient capacity in the target AZ | Check available hosts: `GET /internal/v1/hosts` (mTLS). Retry in a different AZ. |
| `ROOTFS_CREATION_FAILED` | NFS storage pool unreachable or full | Check storage pool health. Check NFS mount availability on hosts. |
| `NETWORK_SETUP_FAILED` | IP pool exhausted or TAP device creation error | Release orphaned IPs. Check host-agent network state. |
| `PROVISION_TIMEOUT` | Generic timeout (30 min) | The reconciler will automatically retry (up to 3 attempts). If all fail, the instance goes to `failed`. |
| `VM_LAUNCH_FAILED` | Firecracker/KVM hypervisor error | Check VM runtime logs on the host. Verify kernel and firecracker binary presence. |

**Recovery:**
- The reconciler retries stuck `INSTANCE_CREATE` jobs up to 3 times.
- After max attempts, the instance transitions to `failed`. Create a new instance.

> **Note:** All error codes follow API_ERROR_CONTRACT_V1. Job error messages are
> safe for user display — no internal hostnames, file paths, or stack traces.

## 2. No SSH Access

**Symptom:** Instance is `running` but SSH connections fail or timeout.

**Checkpoints:**
1. Verify the instance has an IP:
   ```bash
   curl -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$ID | jq '.public_ip'
   ```
2. Verify the SSH key was specified at launch (check `ssh_key_name` field).
3. Check the instance events for `instance.running` — this event is only emitted
   after the SSH readiness check passes (platform polls TCP port 22 before
   declaring `running` per M6 gate).

**Common causes:**

| Issue | Check | Fix |
|---|---|---|
| No public IP | `public_ip` is `null` | Instance was launched without `ssh_key_name` or instance type does not match NAT setup. Phase 1: public IP auto-allocated when `ssh_key_name` is provided. |
| Wrong SSH key | Fingerprint mismatch | Verify key name in `ssh_key_name` field matches your registered key. |
| cloud-init failure | VM booted but cloud-init didn't complete | Check serial console output (host-agent `runtime/console.go`). |
| Security group blocks SSH | Instance launched with networking → SG rules | Ensure the security group attached to the NIC allows inbound TCP/22. |
| SSH readiness timeout | `ssh_readiness_timeout` exceeded | The platform polls TCP port 22 for up to 120s before declaring RUNNING. If SSH doesn't open within that window, the create job fails. |

## 3. No IP Allocated

**Symptom:** Instance is `running` with `public_ip: null` and `private_ip: null`.

**Causes:**
- IP pool for the VPC/availability zone is exhausted.
- Instance was created without networking (`networking` field omitted).
- IP allocation failed during provisioning (check events for `NETWORK_SETUP_FAILED`).

**Resolution:**
1. Check available IPs via DB: query `ip_allocations` for unallocated addresses.
2. Release orphaned IPs from deleted instances (the reconciler sub-scan detects
   ghost IPs but does not auto-correct — see `ip_uniqueness_scan.go`).
3. Create instance in a different VPC/subnet with available IP space.

## 4. Quota Exceeded

**Symptom:** `POST /v1/instances` returns `422 Unprocessable Entity` with
error code `quota_exceeded`.

**Error:**
```json
{
  "error": {
    "code": "quota_exceeded",
    "message": "Instance quota exceeded for this account or project. Delete existing instances or request a quota increase.",
    "target": "instance_type",
    "request_id": "req_..."
  }
}
```

**Resolution:**
1. List current instances and delete unused ones.
2. If using project scope and quota is per-project, create under a different
   project or increase the project quota.
3. Quota is enforced via `CheckAndDecrementQuota` in the resource-manager.

> **Limitation:** Phase 1 quota is count-based (active instance count).
> Reservation-based quota with explicit limits per account/project is planned
> for VM-P2D.

## 5. Insufficient Host Capacity

**Symptom:** Instance creation produces `HOST_UNAVAILABLE` error in async
job result.

**Checkpoints:**
1. List available hosts: `GET /internal/v1/hosts` (requires mTLS — operator-only).
2. Check if target AZ has active hosts with sufficient vCPU/memory/disk.

**Resolution:**
- If no hosts in AZ: wait for host registration or create in a different AZ.
- If hosts exist but are full: terminate unused instances.
- Hosts in `draining`, `drained`, `degraded`, `unhealthy`, `retired`, or
  `fence_required` state are excluded from scheduling.

## 6. Image Not Launchable

**Symptom:** `POST /v1/instances` returns `422` with `image_not_launchable`.

**Causes:**
- Image status is `OBSOLETE`, `FAILED`, or `PENDING_VALIDATION`.
- Platform image without valid cryptographic signature (`signature_valid=false`).
- Image is private and not shared with your principal.

**Resolution:**
1. List images to find launchable ones (ACTIVE or DEPRECATED).
2. Use an image family alias to automatically resolve to the latest launchable version.
3. Request image owner to share the image via grants.

## 7. Stuck Job

**Symptom:** Job status is `in_progress` beyond its timeout.

**Job timeouts by type (from JOB_MODEL_V1 §3):**

| Job Type | Timeout | Max Attempts |
|---|---|---|
| `INSTANCE_CREATE` | 30 min | 3 |
| `INSTANCE_START` | 5 min | 5 |
| `INSTANCE_STOP` | 10 min | 5 |
| `INSTANCE_REBOOT` | 3 min | 5 |
| `INSTANCE_DELETE` | 15 min | 5 |

**Automatic recovery:**
- The **janitor** (60s sweep) detects `in_progress` jobs past their timeout.
- Below `max_attempts`: resets to `pending` for retry.
- At `max_attempts`: marks `dead`, instance → `failed`, message → DLQ.

**Manual inspection:**
```bash
# Check job status
curl -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$ID/jobs/$JOB_ID | jq .
# Key fields: status, attempt_count, max_attempts, error_message
```

Jobs that exhaust all retries are moved to the Dead Letter Queue (DLQ).
DLQ messages require operator review — no automatic re-processing.

## 8. Security Group Blocked Traffic

**Symptom:** Instance is reachable over SSH via direct IP but not over
instance-attached NIC with security group rules.

**Status:** Security group enforcement is implemented in the control plane
(create/list/delete rules) and in the host-agent runtime (iptables rules).
However, end-to-end SG enforcement on live instances requires the VPC network
routes to be wired into the production mux.

**Verification:**
1. List security group rules:
   ```bash
   curl -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/vpcs/$VPC_ID/security_groups/$SG_ID/rules | jq .
   ```
   > Note: VPC endpoints are not yet wired into the production mux.
2. Verify the NIC references the correct SG IDs:
   ```bash
   curl -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$ID | jq '.networking.primary_interface.security_group_ids'
   ```

## 9. Volume Attach Failure

**Symptom:** Volume attachment job fails or volume remains `detached`.

**Checkpoints:**
1. Verify volume exists and is in `available` state:
   ```bash
   curl -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/volumes/$VOL_ID | jq .
   ```
2. Verify instance exists and is in `stopped` or `running` state.
3. Check that the volume's availability zone matches the instance's AZ.
4. Ensure instance has not exceeded `maxVolumesPerInstance` (16).

**Common errors:**
- Volume already attached to another instance.
- Volume in `creating` or `deleting` state.
- Volume AZ mismatch (AZ affinity enforced at attach time).

## 10. Cross-Account / Ownership Errors

**Symptom:** Requests return `404 Not Found` even though the resource exists.

**This is by design.** The platform returns `404` (not `403`) to prevent
existence enumeration (see `AUTH_OWNERSHIP_MODEL_V1 §4`).

**Verification:**
- Confirm the `X-Principal-ID` header matches the resource's `owner_principal_id`.
- Check the resource's ARN in the `owner` field: `arn:cs:compute::principal/<id>`.

## Diagnostic Commands

```bash
# Check API version compatibility
curl -s $BASE/v1/version | jq .

# Check service health
curl -s $BASE/healthz | jq .
# 200 {"status":"ok"} = healthy
# 503 {"status":"degraded","reason":"db_unavailable"} = DB is down

# Get OpenAPI spec for endpoint reference
curl -s $BASE/v1/openapi.json | jq .

# List events for triage
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$ID/events | jq .

# Check resource ownership
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$ID | jq '.owner'

# DB connectivity (operator-only)
# Test from the resource-manager host: check PostgreSQL connectivity via
# the DATABASE_URL configured in the environment.
```

## Event Log Reference

For detailed event types and their meanings, see `EVENTS_SCHEMA_V1.md`.
Key diagnostic events:

| Event | Meaning |
|---|---|
| `instance.provisioning.start` | Worker started provisioning on a host |
| `instance.provisioning.step` | Provisioning step completed (rootfs, network, etc.) |
| `instance.running` | VM confirmed running (SSH readiness verified) |
| `instance.failure` | Instance entered failed state |
| `instance.reconciler.repair` | Reconciler dispatched a repair for drift |
| `instance.ip.uniqueness_violation` | Duplicate IP detected by reconciler sub-scan |
