# EVENTS_SCHEMA_V1

## Instance Events Schema — Phase 1 Contract

**Status:** FROZEN — changes require formal review with downstream dependency analysis.

---

## 1. Purpose

Instance events provide a user-visible audit log of lifecycle transitions and significant occurrences for each instance. They are exposed via `GET /v1/instances/{id}/events` and rendered in the console's Events tab.

Events are distinct from internal structured logs. Events are user-facing, sanitized, and retained per-instance. Logs are operator-facing and centralized.

---

## 2. Database Schema

```sql
CREATE TABLE instance_events (
    id              UUID PRIMARY KEY,
    instance_id     UUID NOT NULL REFERENCES instances(id),
    event_type      VARCHAR(100) NOT NULL,
    message         TEXT NOT NULL,
    actor           VARCHAR(50) NOT NULL,        -- 'USER', 'SYSTEM', 'RECONCILER'
    metadata        JSONB,                        -- optional structured context
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_instance_events_lookup
    ON instance_events (instance_id, created_at DESC);
```

**ID format:** `evt_` prefix + KSUID.

---

## 3. Retention

Phase 1 retains the last **100 events per instance**. Older events are eligible for pruning. Pruning strategy is a background job, not inline deletion.

Deleted instances retain their events for the audit retention period (same as the soft-deleted instance record).

---

## 4. Event Type Catalog (Phase 1)

| Event Type | Trigger | Actor | Message Template |
|------------|---------|-------|-----------------|
| `instance.create` | `POST /v1/instances` accepted | `USER` | Instance creation requested. |
| `instance.provisioning.start` | Worker begins provisioning | `SYSTEM` | Provisioning started on host {host_id}. |
| `instance.provisioning.step` | Each major provisioning step completes | `SYSTEM` | {step_name} completed. |
| `instance.running` | VM confirmed running on hypervisor | `SYSTEM` | Instance is running. |
| `instance.stop.initiate` | `POST :stop` accepted | `USER` | Stop requested. |
| `instance.stopping` | Worker begins stop sequence | `SYSTEM` | Instance is stopping. |
| `instance.stopped` | VM confirmed stopped | `SYSTEM` | Instance stopped. Compute billing paused. |
| `instance.start.initiate` | `POST :start` accepted | `USER` | Start requested. |
| `instance.reboot.initiate` | `POST :reboot` accepted | `USER` | Reboot requested. |
| `instance.rebooting` | Worker begins reboot | `SYSTEM` | Instance is rebooting. |
| `instance.reboot.complete` | VM confirmed running after reboot | `SYSTEM` | Reboot completed. Instance is running. |
| `instance.delete.initiate` | `DELETE /v1/instances/{id}` accepted | `USER` | Deletion requested. |
| `instance.deleting` | Worker begins deletion | `SYSTEM` | Instance deletion in progress. |
| `instance.deleted` | All resources released | `SYSTEM` | Instance deleted. All resources released. |
| `instance.failure` | Instance transitioned to `FAILED` | `SYSTEM` | Instance entered failed state: {error_code}. |
| `instance.reconciler.repair` | Reconciler dispatched a repair action | `RECONCILER` | Drift detected ({drift_class}). Repair action dispatched. |
| `instance.metadata.update` | Labels or name changed via PATCH | `USER` | Instance metadata updated. |

---

## 5. Event Type Rules

- Event types are dot-separated, hierarchical, machine-readable strings.
- Event types are stable — clients and consoles parse these values. Adding new types is non-breaking. Removing or renaming is breaking.
- Each state transition MUST produce exactly one event. Missing events are a bug.
- Events produced by the reconciler use actor `RECONCILER`.

---

## 6. Metadata Field

The optional `metadata` JSONB field provides structured context that varies by event type:

**`instance.failure` metadata:**

```json
{
  "error_code": "PROVISION_TIMEOUT",
  "error_message": "Instance provisioning timed out after 15 minutes."
}
```

**`instance.provisioning.step` metadata:**

```json
{
  "step": "rootfs_materialization",
  "duration_ms": 1250
}
```

**`instance.reconciler.repair` metadata:**

```json
{
  "drift_class": "STUCK_PROVISIONING",
  "repair_job_id": "job_..."
}
```

**Sensitive information policy:** Event metadata must never contain: raw infrastructure errors, internal hostnames beyond `host_id`, database messages, file paths, IP addresses of internal services, or credentials. Events are user-visible.

---

## 7. API Response Shape

`GET /v1/instances/{id}/events`

```json
{
  "events": [
    {
      "id": "evt_2nMpNa7Hk3WXeRCqbGLtx7Z8Gmn",
      "instance_id": "inst_2nMpMz5Ge4VYeRBpaFKsx6Y7Fkn",
      "event_type": "instance.running",
      "message": "Instance is running.",
      "actor": "SYSTEM",
      "metadata": null,
      "created_at": "2026-04-05T12:01:30.123Z"
    }
  ],
  "pagination": {
    "next_page_token": "eyJjdXJzb3IiOiIxMjM0NTY3ODkwIn0=",
    "page_size": 50
  }
}
```

**Pagination:** Cursor-based using `next_page_token`. Default `page_size`: 50. Maximum `page_size`: 100. Events are returned in reverse chronological order (newest first).

---

## 8. Write Path

Events are written to the `instance_events` table in the same database transaction as the state change they describe:

```sql
BEGIN;
  UPDATE instances SET vm_state = 'RUNNING', ... WHERE id = $id AND version = $v;
  INSERT INTO instance_events (id, instance_id, event_type, message, actor, metadata, created_at)
    VALUES ($evt_id, $instance_id, 'instance.running', 'Instance is running.', 'SYSTEM', NULL, NOW());
  -- usage event also in this transaction if applicable
COMMIT;
```

Events are never written outside the state-change transaction. An event without a corresponding state change, or a state change without an event, is a bug.

---

## 9. Invariants

| Rule | Description |
|------|-------------|
| Every state transition produces an event | No silent state changes. |
| Events are written in the same DB transaction as state changes | Ensures consistency. |
| Events are immutable | Once written, events are never modified or deleted (except retention pruning of oldest). |
| Events are user-safe | No sensitive infrastructure details. |
| Event types are stable | Machine-readable, parseable by clients. |
| Actor is always populated | `USER`, `SYSTEM`, or `RECONCILER`. |
