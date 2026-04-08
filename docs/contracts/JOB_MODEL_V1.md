# JOB_MODEL_V1

## Async Job Model and Idempotency — Phase 1 Contract

**Status:** FROZEN — changes require formal review with downstream dependency analysis.

---

## 1. Job Schema

```sql
CREATE TABLE jobs (
    id                  UUID PRIMARY KEY,
    type                VARCHAR(50) NOT NULL,
    instance_id         UUID NOT NULL REFERENCES instances(id),
    status              VARCHAR(20) NOT NULL DEFAULT 'PENDING',
    payload             JSONB NOT NULL,
    result              JSONB,
    idempotency_key     VARCHAR(255),
    attempt_count       INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL DEFAULT 5,
    last_attempted_at   TIMESTAMPTZ,
    timeout_seconds     INTEGER NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_jobs_idempotency_key
    ON jobs (idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE INDEX idx_jobs_pending
    ON jobs (status, created_at ASC) WHERE status = 'PENDING';

CREATE INDEX idx_jobs_instance
    ON jobs (instance_id, created_at DESC);

CREATE INDEX idx_jobs_in_progress_timeout
    ON jobs (status, last_attempted_at ASC) WHERE status = 'IN_PROGRESS';
```

---

## 2. Job State Machine

```
PENDING → IN_PROGRESS → COMPLETED
                      → FAILED
                      → TIMED_OUT

TIMED_OUT → PENDING  (if attempt_count < max_attempts)
          → FAILED   (if attempt_count >= max_attempts → DLQ)
```

| State | Description |
|-------|-------------|
| `PENDING` | Job created, waiting for a worker to claim. |
| `IN_PROGRESS` | Worker has atomically claimed the job and is executing. |
| `COMPLETED` | Job finished successfully. Result stored in `result` JSONB. |
| `FAILED` | Job exhausted retries or hit unrecoverable error. Instance transitions to `FAILED`. |
| `TIMED_OUT` | Job exceeded `timeout_seconds` while `IN_PROGRESS`. Janitor resets to `PENDING` or `FAILED`. |

---

## 3. Job Types and Configuration

| Job Type | `timeout_seconds` | `max_attempts` | Recovery on Timeout |
|----------|-------------------|----------------|---------------------|
| `INSTANCE_CREATE` | 1800 (30 min) | 3 | Reset to `PENDING`, re-enqueue. Worker handles partial create idempotently. |
| `INSTANCE_START` | 300 (5 min) | 5 | Reset to `PENDING`, re-enqueue. |
| `INSTANCE_STOP` | 600 (10 min) | 5 | Reset to `PENDING`, re-enqueue. |
| `INSTANCE_REBOOT` | 180 (3 min) | 5 | Reset to `PENDING`, re-enqueue. |
| `INSTANCE_DELETE` | 900 (15 min) | 5 | Reset to `PENDING`, re-enqueue. Worker handles partial delete idempotently. |

---

## 4. Atomic Job Claim

Workers claim jobs with an atomic UPDATE. This is the distributed lock mechanism.

```sql
UPDATE jobs
SET status = 'IN_PROGRESS',
    attempt_count = attempt_count + 1,
    last_attempted_at = NOW(),
    updated_at = NOW()
WHERE id = $job_id
  AND status = 'PENDING';
```

If zero rows affected → job already claimed or not pending → skip.

**Invariant:** At most one worker processes a given job at a time. The atomic UPDATE guarantees this without external locking.

---

## 5. Job Dispatch Contract

The API server creates and dispatches jobs in a specific order:

```
1. BEGIN TRANSACTION
2. INSERT INTO instances (id, ..., vm_state='REQUESTED', locked_by=$job_id)
3. INSERT INTO jobs (id, type, instance_id, status='PENDING', payload, idempotency_key)
4. INSERT INTO idempotency_keys (key, job_id, response, created_at)
5. COMMIT
6. Enqueue job_id to message queue  ← outside transaction
7. Return 202 Accepted to client
```

**Crash recovery:** If the API crashes between COMMIT (step 5) and enqueue (step 6), the reconciler's stuck-job scan finds the `PENDING` job with no queue message and re-enqueues it.

**Worker consumption:**

```
1. Dequeue job_id from message queue
2. SELECT * FROM jobs WHERE id = $job_id
3. Atomic claim (UPDATE ... SET status='IN_PROGRESS')
4. Execute job logic via Host Agent
5. UPDATE jobs SET status='COMPLETED', result=$result
6. ACK message from queue  ← only after successful DB update
```

Workers fetch full payload from DB, not from queue message body. Queue message contains only the `job_id`.

---

## 6. Idempotency — Request Level

**Mechanism:** Client provides `Idempotency-Key` header (UUID format) on all mutating API requests.

**Idempotency keys table:**

```sql
CREATE TABLE idempotency_keys (
    key             VARCHAR(255) PRIMARY KEY,
    job_id          UUID NOT NULL REFERENCES jobs(id),
    response_code   INTEGER NOT NULL,
    response_body   JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**Rules:**

| Scenario | Behavior | HTTP Response |
|----------|----------|---------------|
| Key not found | Process request normally. Store key + response after commit. | Varies (normally 202) |
| Key found, payload matches original | Return cached response. Do not create new job. | Original response (202) |
| Key found, payload differs | Reject. | `409 Conflict`, code `idempotency_key_mismatch` |
| Key absent on mutating request | Reject. | `400 Bad Request`, code `missing_idempotency_key` |

**Key expiry:** 24 hours. Keys older than 24 hours are eligible for cleanup. A reused key after expiry is treated as a new request.

**Payload comparison:** The server compares the serialized request body hash (SHA-256) of the new request against the stored hash of the original request. Field-level comparison is not performed.

---

## 7. Idempotency — Worker Level

All worker operations are idempotent by contract. Workers receive a `job_id` in every queue message and look up full state from DB.

| Operation | Idempotency Rule |
|-----------|-----------------|
| `CreateInstance` on existing VM | Detect existing VM on hypervisor (by `instance_id`), report current state, succeed. |
| `DeleteInstance` on absent VM | Detect absence, succeed. |
| `StartInstance` on running VM | No-op, succeed. |
| `StopInstance` on stopped VM | No-op, succeed. |
| IP allocation for already-allocated instance | Return existing allocation. |
| Rootfs creation when CoW overlay already exists | Adopt existing disk. |

---

## 8. Job Timeout Janitor

A background process runs periodically (every 60 seconds) to detect stuck jobs:

```sql
SELECT id, attempt_count, max_attempts FROM jobs
WHERE status = 'IN_PROGRESS'
  AND last_attempted_at + (timeout_seconds * INTERVAL '1 second') < NOW();
```

**For each stuck job:**

```
IF attempt_count < max_attempts:
    UPDATE jobs SET status = 'TIMED_OUT', updated_at = NOW()
    -- Reconciler or janitor resets to PENDING and re-enqueues
ELSE:
    UPDATE jobs SET status = 'FAILED', updated_at = NOW()
    UPDATE instances SET vm_state = 'FAILED', locked_by = NULL WHERE id = $instance_id
    -- Move message to DLQ
```

---

## 9. Dead Letter Queue (DLQ)

Jobs that exceed `max_attempts` are moved to the DLQ.

**DLQ handling rules:**
- DLQ receipt triggers an alert. Any DLQ message indicates a persistent processing bug.
- DLQ messages are never automatically re-queued.
- Manual operator review is required. Resolution: fix bug, then manually re-process or mark as resolved.
- Instance remains in `FAILED` state until operator intervention.

---

## 10. Invariants

| Rule | Description |
|------|-------------|
| Job payload is read from DB, not queue | Queue message carries only `job_id`. Prevents stale/tampered payloads. |
| Idempotency key checked before job creation | First operation in the API handler, before any DB write. |
| ACK only after successful DB update | If DB update fails, message returns to queue for retry. |
| One active job per instance | Enforced by `locked_by` on instances table. Second job attempt returns 409. |
| Finite retry with DLQ | Jobs never retry indefinitely. `max_attempts` is enforced. |
| Compensating actions are idempotent | Every rollback step uses delete-if-exists / release-if-held semantics. |

---

## 11. Open Implementation Decisions

| ID | Question | Impact |
|----|----------|--------|
| OID-JM-1 | Exact `max_attempts` per job type: should it be configurable at runtime or hardcoded? | Operational flexibility vs. complexity. |
| OID-JM-2 | DLQ operational runbook: who reviews, what is the SLA, how are messages reprocessed? | Operational process definition. |
| OID-JM-3 | Is `timeout_seconds` per-type or per-job-instance? Master Blueprint lists per-type. | Schema and janitor logic. |
