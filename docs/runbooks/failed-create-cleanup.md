# Failed Create Cleanup Runbook

## Overview

This runbook covers detection, diagnosis, and cleanup of failed VM create operations. A create can fail at multiple points in the provisioning pipeline, each requiring different cleanup steps.

## Provisioning Pipeline Failure Points

The `INSTANCE_CREATE` worker executes these steps in order. Any step failure triggers rollback in reverse order.

| Step | Operation | Failure Mode | State Left | Quota Impact |
|---|---|---|---|---|
| 1 | DB: requested → provisioning | Optimistic lock failure | requested (retried) | None |
| 2 | Scheduler: SelectHost | No available hosts | provisioning | Freed when marked failed |
| 3 | DB: AssignHost | Concurrent mod | provisioning | Freed when marked failed |
| 3b | DB: CreateRootDisk | DB write failure | provisioning (no disk) | Freed when marked failed |
| 4 | Network: AllocateIP | Pool exhausted / DB error | provisioning (no IP) | Freed when marked failed |
| 5 | Host Agent: CreateInstance | Runtime failure / gRPC error | provisioning (IP released) | Freed when marked failed |
| 6 | Readiness: waitForSSH | SSH timeout (>120s) | provisioning (VM deleted) | Freed when marked failed |
| 7 | DB: provisioning → running | Optimistic lock failure | provisioning (VM live) | None (retry safe) |

## Error Code Separation

Two distinct failure classes for admission denial:

| Error | HTTP | Meaning | Client Action |
|---|---|---|---|
| `quota_exceeded` | 422 | Count-based admission: scope has max instances | Delete existing instances or request quota increase |
| `insufficient_capacity` | 503 | Platform capacity: no ready host has resources | Retry with backoff; operation may contact support |

These must never be confused. Quota errors are client-correctable (422). Capacity errors are platform-side conditions (503).

## Detecting Failed Creates

### 1. Check Instance State

Search for stuck provisioning instances:
```
GET /v1/instances?status=provisioning
```

The instance will be in `provisioning` state with an `instance.failure` event containing the specific failure reason.

### 2. Read Failure Events

```
GET /v1/instances/{instance_id}/events
```

Look for `instance.failure` events with messages like:
- `boot_failure: step2: no available hosts`
- `boot_failure: step4 allocate IP: <reason>`
- `boot_failure: step5 CreateInstance: <reason>`
- `boot_failure: step6 readiness: SSH port did not open`

### 3. Check Worker Logs

The worker logs each step with a `job_id` and `instance_id` prefix. Search for:
```
INSTANCE_CREATE: starting
step1: instance provisioning
step2: host selected  (or failure)
step3: host assigned  (or failure)
step4: IP allocated   (or failure)
step5: VM running on host  (or failure)
step6: VM ready       (or failure)
step7: completed      (or failure)
```

## Cleanup Procedures

### Scenario 1: Instance Stuck in `requested`

**Cause:** The create job was never claimed by a worker (disconnected worker, DB error during job insert).

**Resolution:**
1. No runtime resources exist (no host, no IP, no VM)
2. Delete the instance: `DELETE /v1/instances/{instance_id}`
3. If delete is not possible (state machine rejects), the reconciler will mark it `failed` after 5 minutes

### Scenario 2: Instance Stuck in `provisioning`

**Cause:** Worker partially provisioned and failed mid-flow.

**Sub-cases:**

#### 2a. No Host Assigned (Step 2 Failure)
- No runtime resources
- Reconciler marks failed after 15 minutes
- Or: manually delete the instance to free quota immediately

#### 2b. Host Assigned, No IP (Step 4 Failure)
- Host is assigned (host_id set), no IP allocated
- `failInstance` transitions to `failed`
- Quota freed (CountActiveInstancesByScope excludes `failed`)
- The host resource usage may be stale; heartbeat will correct it

#### 2c. IP Allocated, CreateInstance Failed (Step 5 Failure)
- IP was released in rollback (step 5 calls `ReleaseIP`)
- Instance is `failed`
- No runtime VM exists
- Root disk DB record may exist; reconciled on delete

#### 2d. VM Created, Readiness Failed (Step 6 Failure)
- VM was deleted in rollback (`DeleteInstance`)
- IP was released in rollback (`ReleaseIP`)
- Instance is `failed`
- Root disk DB record may exist; reconciled on delete

### Scenario 3: No Available Hosts (Capacity Failure)

**Cause:** All ready hosts are at capacity. This is a platform capacity shortage, not a client error.

**Resolution:**
1. Add capacity (register new hosts)
2. Drain and reactivate maintenance hosts
3. Clients receive `insufficient_capacity` (503) and should retry with exponential backoff

The instance is immediately marked `failed` by the worker with reason `"step2: no available hosts"`.

### Scenario 4: Quota Exceeded

**Cause:** The scope has reached its maximum instance count. No job is created, no instance row exists.

**Response shape:**
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
1. List current instances: `GET /v1/instances`
2. Delete unused instances
3. Or request a quota increase (operator DB write on `project_quotas` table)

## Quota Leak Prevention

The platform prevents quota leaks through:

1. **Count-based model:** Quota is derived from `SELECT COUNT(*) FROM instances WHERE vm_state NOT IN ('deleted', 'failed')`. No reservation column to leak.

2. **Failed instance exclusion:** Any instance in `failed` state is excluded from the active count. The reconciler and janitor transition stuck instances to `failed`.

3. **Delete exclusion:** Soft-deleted instances (`deleted_at IS NOT NULL`, `vm_state='deleted'`) are excluded.

4. **RefundQuota seam:** The `RefundQuota` method exists as a forward-compatible API seam for future reservation-based quota models. In the count-based model, it's a no-op.

## Automated Cleanup

### Reconciler (5-minute resync + event-driven)
- `stuck_provisioning` (15+ min in provisioning) → marks instance `failed`
- `stuck_provisioning` (5+ min in requested) → marks instance `failed`

### Janitor (60-second sweep)
- Timed-out in_progress jobs below max_attempts → reset to pending
- Timed-out jobs at max_attempts → mark dead + fail instance

## Manual Cleanup Command Reference

```bash
# Delete a stuck instance (marks deleting, worker tears down)
curl -X DELETE http://rm/v1/instances/{instance_id} \
  -H "X-Principal-ID: {principal_id}"

# Force-transition to failed (via internal CLI)
internal-cli fail-instance --instance-id {instance_id}
```
