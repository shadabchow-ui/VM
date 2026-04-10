# H2-TG3: DLQ Configuration Documentation

**Gate Item:** Q-3  
**Runbook Step:** 13  
**Date:** ____________________  
**Operator:** ____________________

---

## DLQ Threshold N

| Parameter | Value | Source |
|-----------|-------|--------|
| **DLQ Threshold (N)** | 3 | `db/migrations/001_initial.up.sql` line 114: `max_attempts INTEGER NOT NULL DEFAULT 3` |
| **DLQ Status Value** | `'dead'` | `db/migrations/001_initial.up.sql` line 120-123: `status IN ('pending','in_progress','completed','failed','dead')` |
| **Retry Logic Location** | `packages/queue/consumer.go` | `Fail()` function lines 116-128 |

---

## Code References

### Schema Definition (001_initial.up.sql)
```sql
CREATE TABLE jobs (
    ...
    attempt_count   INTEGER      NOT NULL DEFAULT 0,
    max_attempts    INTEGER      NOT NULL DEFAULT 3,
    ...
    CONSTRAINT jobs_status_check CHECK (
        status IN ('pending','in_progress','completed','failed','dead')
    ),
    ...
);
```

### Fail Function (packages/queue/consumer.go)
```go
// Fail marks a claimed job as failed with an error message.
// If attempt_count >= max_attempts the job transitions to 'dead' (DLQ).
func Fail(ctx context.Context, pool Pool, jobID, errMsg string) error {
    now := time.Now().UTC()
    _, err := pool.Exec(ctx, `
        UPDATE jobs
        SET status        = CASE
                              WHEN attempt_count >= max_attempts THEN 'dead'
                              ELSE 'pending'
                            END,
            error_message = $2,
            updated_at    = $3
        WHERE id = $1
    `, jobID, errMsg, now)
    return err
}
```

### DLQ Consumer (packages/queue/dlq.go)
```go
// DrainDead reads all status='dead' jobs, logs each one, and marks them acknowledged.
func (d *DLQConsumer) DrainDead(ctx context.Context) error {
    rows, err := d.pool.Query(ctx, `
        SELECT id, instance_id, job_type, error_message, attempt_count, updated_at
        FROM jobs
        WHERE status = 'dead'
        ORDER BY updated_at ASC
        LIMIT 100
    `)
    // ...logs at ERROR level for operator attention
}
```

---

## Retry Behavior

| Attempt | Status After | Notes |
|---------|--------------|-------|
| 1 | `pending` (if failed) | First attempt failed, re-queued |
| 2 | `pending` (if failed) | Second attempt failed, re-queued |
| 3 | `dead` | Third attempt failed, moved to DLQ |

**Retry Interval:** Controlled by worker poll interval (no exponential backoff in Phase 1).

**Total Elapsed Time:** Approximately `3 × poll_interval + 3 × handler_execution_time`

---

## Verification Queries

```sql
-- Confirm max_attempts column exists and has correct default
SELECT column_name, column_default, is_nullable
FROM information_schema.columns
WHERE table_name = 'jobs' AND column_name = 'max_attempts';

-- Expected output:
-- column_name  | column_default | is_nullable
-- max_attempts | 3              | NO

-- Confirm 'dead' is a valid status
SELECT constraint_name, check_clause
FROM information_schema.check_constraints
WHERE constraint_name = 'jobs_status_check';
```

---

## Sign-Off

- [ ] DLQ threshold N documented: **3**
- [ ] DLQ status value documented: **dead**
- [ ] Retry interval documented: **worker poll interval**
- [ ] Code references verified

**Verified by:** ____________________  
**Date:** ____________________
