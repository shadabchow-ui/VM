# Compute Platform — Operations Timeline

This document describes how operators and users can track operations across
the platform using the events and jobs endpoints.

## API Endpoints

### Instance events: `GET /v1/instances/{id}/events`

Returns up to 100 events per instance, newest first. Events are immutable and
written atomically in the same transaction as state changes.

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$ID/events | jq .
```

**Response:**

```json
{
  "events": [
    {
      "id": "evt_2nMpMz5Ge4VYeRBpaFKsx6Y7Fkn",
      "event_type": "instance.running",
      "message": "Instance is running.",
      "actor": "SYSTEM",
      "created_at": "2026-04-05T12:01:30.123Z"
    }
  ],
  "total": 1
}
```

**Event types (EVENTS_SCHEMA_V1):**

| Event Type | Trigger | Actor |
|---|---|---|
| `instance.create` | POST /v1/instances accepted | USER |
| `instance.provisioning.start` | Worker begins provisioning | SYSTEM |
| `instance.provisioning.step` | Each major step completes | SYSTEM |
| `instance.running` | VM confirmed running (SSH ready) | SYSTEM |
| `instance.stop.initiate` | POST :stop accepted | USER |
| `instance.stopping` | Worker begins stop sequence | SYSTEM |
| `instance.stopped` | VM confirmed stopped | SYSTEM |
| `instance.start.initiate` | POST :start accepted | USER |
| `instance.reboot.initiate` | POST :reboot accepted | USER |
| `instance.rebooting` | Worker begins reboot | SYSTEM |
| `instance.reboot.complete` | VM rebooted, running again | SYSTEM |
| `instance.delete.initiate` | DELETE accepted | USER |
| `instance.deleting` | Worker begins deletion | SYSTEM |
| `instance.deleted` | All resources released | SYSTEM |
| `instance.failure` | Instance → FAILED | SYSTEM |
| `instance.reconciler.repair` | Reconciler dispatched repair | RECONCILER |
| `instance.metadata.update` | Labels/name changed via PATCH | USER |
| `instance.ip.uniqueness_violation` | Duplicate IP detected | SYSTEM |

### Job status: `GET /v1/instances/{id}/jobs/{job_id}`

Returns the current state of an async operation (create/delete/stop/start/reboot).

```bash
curl -s -H "X-Principal-ID: $PRINCIPAL" \
  $BASE/v1/instances/$ID/jobs/$JOB_ID | jq .
```

**Response:**

```json
{
  "id": "job_2nMpMz5Ge4VYeRBpaFKsx6Y7Fkn",
  "instance_id": "inst_2nMpMz5Ge4VYeRBpaFKsx6Y7Fkn",
  "job_type": "INSTANCE_CREATE",
  "status": "completed",
  "attempt_count": 1,
  "max_attempts": 3,
  "error_message": null,
  "created_at": "2026-04-05T12:00:00.000Z",
  "updated_at": "2026-04-05T12:01:30.123Z",
  "completed_at": "2026-04-05T12:01:30.123Z"
}
```

**Job lifecycle (JOB_MODEL_V1):**

```
PENDING → IN_PROGRESS → COMPLETED
                      → FAILED
                      → TIMED_OUT

TIMED_OUT → PENDING  (if attempt_count < max_attempts)
          → FAILED   (if attempt_count >= max_attempts → DLQ)
```

**Job types and timeouts:**

| Job Type | Timeout | Max Attempts |
|---|---|---|
| INSTANCE_CREATE | 30 min | 3 |
| INSTANCE_START | 5 min | 5 |
| INSTANCE_STOP | 10 min | 5 |
| INSTANCE_REBOOT | 3 min | 5 |
| INSTANCE_DELETE | 15 min | 5 |

## Tracking a Full Lifecycle

### 1. Create instance → Running

```bash
# Create
RESP=$(curl -s -X POST \
  -H "X-Principal-ID: $PRINCIPAL" \
  -H "Content-Type: application/json" \
  $BASE/v1/instances \
  -d '{"name":"demo","instance_type":"gp1.small","image_id":"...","availability_zone":"us-east-1a","ssh_key_name":"my-key"}')
INSTANCE_ID=$(echo "$RESP" | jq -r '.instance.id')

# Poll status until running
while true; do
  STATUS=$(curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID | jq -r '.status')
  echo "Status: $STATUS"
  [ "$STATUS" = "running" ] && break
  [ "$STATUS" = "failed" ] && echo "Provisioning failed" && exit 1
  sleep 5
done

# View all events
curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID/events | jq .
```

### 2. Stop → Start cycle

```bash
# Stop
JOB=$(curl -s -X POST -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID/stop)
JOB_ID=$(echo "$JOB" | jq -r '.job_id')

# Wait for stopped
while true; do
  STATUS=$(curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID/jobs/$JOB_ID | jq -r '.status')
  [ "$STATUS" = "completed" ] && break
  sleep 3
done

# Start
JOB=$(curl -s -X POST -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID/start)
JOB_ID=$(echo "$JOB" | jq -r '.job_id')

# Wait for running
while true; do
  STATUS=$(curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID/jobs/$JOB_ID | jq -r '.status')
  [ "$STATUS" = "completed" ] && break
  sleep 3
done
```

### 3. Delete

```bash
JOB=$(curl -s -X DELETE -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID)
JOB_ID=$(echo "$JOB" | jq -r '.job_id')

while true; do
  STATUS=$(curl -s -H "X-Principal-ID: $PRINCIPAL" $BASE/v1/instances/$INSTANCE_ID/jobs/$JOB_ID | jq -r '.status')
  [ "$STATUS" = "completed" ] && break
  sleep 3
done
```

## Reconciler Timeline

The reconciler operates on two triggers:

1. **Event-driven** (`Enqueue`): triggered by API handler after any state
   mutation. Non-blocking (drops on full channel).

2. **Periodic resync** (5-minute interval): full scan of all active instances.
   Classifies drift and dispatches repair jobs.

**Drift classes (classifier.go):**

| Drift Class | Threshold | Repair |
|---|---|---|
| `stuck_provisioning` | PROVISIONING > 15 min or REQUESTED > 5 min | INSTANCE_CREATE repair job |
| `wrong_runtime_state` | STOPPING > 10 min, REBOOTING > 3 min, DELETING > 10 min | INSTANCE_STOP/REBOOT/DELETE repair job |
| `missing_runtime_process` | RUNNING with stale update > 5 min | Fail instance |
| `orphaned_resource` | RUNNING with no host | Fail instance |
| `job_timeout` | Job IN_PROGRESS past timeout | Janitor requeue/fail |

> **Current limitation:** Reconciler deployment activation is milestone-gated.
> Distributed HA reconciler with lease management is Phase 2. The reconciler is
> code-complete and fully tested but not deployed as a daemon in this phase.

## Janitor Timeline

The job timeout janitor (`services/reconciler/janitor.go`) runs every 60 seconds:

1. Scans for `in_progress` jobs past their per-type timeout.
2. Below `max_attempts` → resets to `pending`.
3. At `max_attempts` → marks `dead`, instance → `failed`, emits `instance.failure`.

## IP Uniqueness Sub-Scan

Runs every 5 minutes (same cadence as reconciler resync):

1. Queries `ip_allocations` for duplicate `(ip_address, vpc_id)` pairs.
2. Logs each anomaly at ERROR level.
3. Writes `instance.ip.uniqueness_violation` events.
4. **Never mutates** — detection only.

## Known Limitations

- **No console UI for events/jobs**: Events and jobs are only available via API.
  The console (Phase 16B) provides instance lists and detail views but does not
  yet render the event timeline.
- **No push notifications/webhooks**: All status tracking is pull-based
  (poll job status or instance status). No webhook/callback mechanism exists.
- **No event search/filter API**: Events are returned newest-first with a fixed
  limit (100). There is no server-side filtering, date range queries, or
  pagination beyond the MAX_LIMIT.
- **Reconciler not deployed**: Although the reconciler is code-complete and
  tested, daemon-level activation is milestone-gated. Until deployment,
  reconciliation and janitor behaviour is proven at the test seam only.
