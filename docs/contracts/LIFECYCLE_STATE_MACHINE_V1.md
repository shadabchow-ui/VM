# LIFECYCLE_STATE_MACHINE_V1

## Canonical Lifecycle State Machine — Phase 1 Contract

**Status:** FROZEN — changes require formal review with downstream dependency analysis.

---

## 1. State Enum

Exactly 9 states. No additions in Phase 1.

```
REQUESTED | PROVISIONING | RUNNING | STOPPING | STOPPED | REBOOTING | DELETING | DELETED | FAILED
```

**Stable states** (system is at rest, user can act):
- `RUNNING` — VM up, network active, compute billing active.
- `STOPPED` — VM down, root disk preserved on NFS, compute billing stopped, storage billing continues.
- `FAILED` — Terminal error. Only valid user action is `Delete`.

**Transitional states** (system is converging, user cannot act except `Delete` where noted):
- `REQUESTED` — API validated and persisted, job not yet dispatched. Extremely short-lived.
- `PROVISIONING` — Scheduler placement, IP allocation, rootfs creation, TAP setup, Firecracker launch in progress.
- `STOPPING` — ACPI shutdown sent, waiting for VM power-off.
- `REBOOTING` — Reboot issued, same host, IP retained.
- `DELETING` — Resource teardown in progress.

**Terminal state:**
- `DELETED` — All `delete_on_termination=true` resources destroyed. Record retained for audit, not visible in standard list calls.

---

## 2. State Transition Table

Rows = current state. Columns = target state. `✓ (user)` = user-initiated via API. `✓ (system)` = system-initiated by worker/reconciler. `F` = automatic transition on unrecoverable error. `—` = illegal.

| From \ To | REQUESTED | PROVISIONING | RUNNING | STOPPING | STOPPED | REBOOTING | DELETING | DELETED | FAILED |
|-----------|-----------|-------------|---------|----------|---------|-----------|----------|---------|--------|
| **REQUESTED** | — | ✓ (system) | — | — | — | — | ✓ (user) | — | F |
| **PROVISIONING** | — | — | ✓ (system) | — | — | — | ✓ (user) | — | F |
| **RUNNING** | — | — | — | ✓ (user) | — | ✓ (user) | ✓ (user) | — | F |
| **STOPPING** | — | — | — | — | ✓ (system) | — | ✓ | — | F |
| **STOPPED** | — | ✓ (user:start) | — | — | — | — | ✓ (user) | — | — |
| **REBOOTING** | — | — | ✓ (system) | — | — | — | ✓ | — | F |
| **DELETING** | — | — | — | — | — | — | — | ✓ (system) | F |
| **FAILED** | — | — | — | — | — | — | ✓ (user) | — | — |

---

## 3. Transition Validator Function

The state machine validator is a shared package (`packages/state-machine/`). It is called by the API server, worker, and reconciler before any state-writing DB operation.

**Signature:**

```
validate_transition(current_state, action) → (allowed: bool, next_state: State | error: TransitionError)
```

**Action-to-transition mapping:**

| Action | Allowed From | Next State |
|--------|-------------|------------|
| `CREATE` | (new instance) | `REQUESTED` |
| `DISPATCH` | `REQUESTED` | `PROVISIONING` |
| `PROVISION_COMPLETE` | `PROVISIONING` | `RUNNING` |
| `STOP` | `RUNNING` | `STOPPING` |
| `STOP_COMPLETE` | `STOPPING` | `STOPPED` |
| `START` | `STOPPED` | `PROVISIONING` |
| `REBOOT` | `RUNNING` | `REBOOTING` |
| `REBOOT_COMPLETE` | `REBOOTING` | `RUNNING` |
| `DELETE` | `REQUESTED`, `PROVISIONING`, `RUNNING`, `STOPPING`, `STOPPED`, `REBOOTING`, `FAILED` | `DELETING` |
| `DELETE_COMPLETE` | `DELETING` | `DELETED` |
| `FAIL` | `REQUESTED`, `PROVISIONING`, `STOPPING`, `REBOOTING`, `DELETING` | `FAILED` |

**Illegal transition behavior:**
- Validator returns `(allowed: false, error: ILLEGAL_STATE_TRANSITION)`.
- API returns `409 Conflict` with error code `illegal_state_transition`.
- The DB write is not executed.

---

## 4. State Invariants

| Rule | Description |
|------|-------------|
| No direct `RUNNING → STOPPED` | Must transit through `STOPPING`. |
| No direct `REQUESTED → RUNNING` | Must transit through `PROVISIONING`. |
| `FAILED` accepts only `DELETE` | All other actions return `409`. |
| `DELETED` accepts nothing | Terminal. No further transitions. |
| `DELETING` accepts nothing from users | System can complete to `DELETED` or `FAIL` to `FAILED`. |
| Transitional states block user actions | `PROVISIONING`, `STOPPING`, `REBOOTING`, `DELETING` — no user-initiated lifecycle actions except `DELETE` from `PROVISIONING`, `STOPPING`, `REBOOTING`. |

---

## 5. Failure Transitions

Any transitional state can transition to `FAILED` on unrecoverable error. The transition to `FAILED` is always system-initiated.

**Failure timeouts:**

| Scenario | Timeout | Behavior |
|----------|---------|----------|
| Stuck in `PROVISIONING` | 15 minutes | Transition to `FAILED`. Release reserved IP, delete partial disk, emit cleanup job. No auto-retry. |
| Stop failure — soft phase | 5 minutes from stop initiation | Issue hard destroy (`virsh destroy` equivalent) to hypervisor. |
| Stop failure — hard phase | 10 minutes total | If hypervisor does not confirm destroy, transition to `FAILED` with code `STOP_FORCE_FAILED`. Operator alert. |
| Stuck in `REBOOTING` | 3 minutes | Transition to `FAILED` with code `REBOOT_TIMEOUT`. |
| Delete failure | Per job `max_attempts` | Retry N times, then `FAILED` with code `DELETE_FAILED`. Manual operator intervention required. |
| Stuck in `REQUESTED` | 5 minutes | Reconciler detects stuck job, transitions to `FAILED`. |

---

## 6. Usage Event Coupling

State transitions that affect billing MUST emit usage events in the same database transaction:

| Transition | Usage Event | Transaction Rule |
|------------|-------------|------------------|
| `* → RUNNING` | `start_usage` | Same DB transaction as `vm_state = 'RUNNING'` write. |
| `RUNNING → STOPPING` (completed to `STOPPED`) | `end_usage` | Same DB transaction as `vm_state = 'STOPPED'` write. |
| `* → DELETED` | `end_usage` | Same DB transaction as `vm_state = 'DELETED'` write. (Only if instance was `RUNNING` at delete initiation.) |
| `* → FAILED` (from `RUNNING` via crash) | `end_usage` | Same DB transaction as `vm_state = 'FAILED'` write. |

Violation of this coupling is an existential business risk — billing gaps cannot be recovered without audit reconstruction.

---

## 7. Optimistic Locking

All state-writing DB operations use the `version` column:

```sql
UPDATE instances
SET vm_state = $new_state, version = version + 1, updated_at = NOW()
WHERE id = $id AND version = $expected_version;
```

If zero rows affected → stale version → operation rejected. Caller must re-read and retry or abort.

---

## 8. Instance Locking

At most one state-mutating job is active per instance at a time:

```sql
UPDATE instances
SET locked_by = $job_id
WHERE id = $id AND locked_by IS NULL AND version = $expected_version;
```

If zero rows affected → another job holds the lock → return `409 Conflict`.

Lock is released when the job completes (success or failure):

```sql
UPDATE instances
SET locked_by = NULL, version = version + 1
WHERE id = $id AND locked_by = $job_id;
```

---

## 9. Required Test Matrix

Every cell must have a passing test before the state machine is considered complete:

| Current State | CREATE | START | STOP | REBOOT | DELETE | FAIL |
|--------------|--------|-------|------|--------|--------|------|
| REQUESTED | — | ✗ illegal | ✗ illegal | ✗ illegal | ✓ legal | ✓ legal |
| PROVISIONING | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal | ✓ legal | ✓ legal |
| RUNNING | ✗ illegal | ✗ illegal | ✓ legal | ✓ legal | ✓ legal | ✓ legal |
| STOPPING | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal | ✓ legal | ✓ legal |
| STOPPED | ✗ illegal | ✓ legal | ✗ illegal | ✗ illegal | ✓ legal | ✗ illegal |
| REBOOTING | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal | ✓ legal | ✓ legal |
| DELETING | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal | ✓ legal |
| DELETED | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal |
| FAILED | ✗ illegal | ✗ illegal | ✗ illegal | ✗ illegal | ✓ legal | ✗ illegal |

All `✗ illegal` cells must return a structured error. All `✓ legal` cells must produce the correct `next_state`.
