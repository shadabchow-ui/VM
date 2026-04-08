# M5 Status

## Overview

M5 — API Contract Complete — is implemented across three passes on the macOS dev box.
Linux/KVM hardware validation remains deferred (same as M2/M3/M4).

---

## PASS 1 — Complete

**Scope:** Core public instance API endpoints.

- `POST /v1/instances` — create instance, returns 202 + instance resource
- `GET /v1/instances` — list instances scoped to principal
- `GET /v1/instances/{id}` — get single instance

**Files changed:** `instance_handlers.go`, `instance_types.go`, `instance_errors.go`,
`instance_validation.go`, `instance_handlers_test.go`, `api.go`, `main.go`.

---

## PASS 2 — Complete

**Scope:** Auth middleware, ownership enforcement, lifecycle action endpoints.

- `requirePrincipal` middleware — enforces `X-Principal-ID` header; 401 on missing/empty
- `loadOwnedInstance` — 404 on non-existent or cross-account access (no existence leakage)
- `DELETE /v1/instances/{id}` — enqueues `INSTANCE_DELETE` job, returns 202
- `POST /v1/instances/{id}/stop` — enqueues `INSTANCE_STOP` job, returns 202
- `POST /v1/instances/{id}/start` — enqueues `INSTANCE_START` job, returns 202
- `POST /v1/instances/{id}/reboot` — enqueues `INSTANCE_REBOOT` job, returns 202
- State machine validation before job enqueue; 409 `illegal_state_transition` on invalid transitions

**Files changed:** `instance_auth.go`, `instance_handlers.go`, `instance_handlers_test.go`.

---

## PASS 3 — Complete

**Scope:** Idempotency-Key support, job status endpoint, this status doc.

### Idempotency-Key support

Implemented for all mutating endpoints. When the `Idempotency-Key` header is present,
a composite key `{principalID}:{key}:{scope}` is used for deduplication. When absent,
current behavior is preserved unchanged (no error, no deduplication).

**POST /v1/instances (create):**
Composite key `{principalID}:{key}:create` is stored as an `INSTANCE_CREATE` sentinel
job upon first request. Duplicate requests return the original instance response (202).

**POST /v1/instances/{id}/stop|start|reboot, DELETE /v1/instances/{id}:**
Composite key `{principalID}:{key}:{jobType}`. Duplicate request returns the original
`LifecycleResponse` (202). Same key reused for a **different** instance returns 409
`idempotency_key_mismatch`.

### Job status endpoint

**GET /v1/instances/{id}/jobs/{job_id}** — returns 202 + `JobResponse`.

- Ownership enforced: instance must be owned by calling principal (404 on mismatch)
- Instance/job relationship validated: job must have `instance_id = {id}` (404 otherwise)
- Response fields: `id`, `instance_id`, `job_type`, `status`, `attempt_count`,
  `max_attempts`, `error_message`, `created_at`, `updated_at`, `completed_at`
- Not exposed: `idempotency_key`, `claimed_at` (internal-only per JOB_MODEL_V1)

### New repo method

`GetJobByInstanceAndID(ctx, instanceID, jobID)` — fetches a job constrained by both
`id` and `instance_id`; returns `nil, nil` on miss.

**Files changed:**
- `services/resource-manager/instance_handlers.go`
- `services/resource-manager/instance_handlers_test.go`
- `services/resource-manager/instance_types.go`
- `services/resource-manager/instance_errors.go`
- `internal/db/job_repo.go`

---

## Test counts (PASS 3 additions)

| Test | Coverage |
|------|---------|
| `TestIdempotency_Create_SameKey` | Duplicate create returns same instance |
| `TestIdempotency_Create_DifferentKey` | Different key → new instance |
| `TestIdempotency_Create_NoKey` | No key → normal behavior |
| `TestIdempotency_Stop_SameKey` | Duplicate stop returns same job_id |
| `TestIdempotency_Start_SameKey` | Duplicate start returns same job_id |
| `TestIdempotency_Reboot_SameKey` | Duplicate reboot returns same job_id |
| `TestIdempotency_Lifecycle_DifferentKey` | Different key → distinct job |
| `TestIdempotency_Lifecycle_SameKeyDifferentInstance` | Same key + different instance → 409 |
| `TestIdempotency_Lifecycle_NoKey` | No key → normal behavior |
| `TestGetJob_Happy` | Job status happy path |
| `TestGetJob_ResponseShape` | All required fields present |
| `TestGetJob_NotFound` | 404 + job_not_found code |
| `TestGetJob_WrongInstanceJobPairing` | 404 on wrong instance/job pairing |
| `TestGetJob_WrongOwner` | 404 on cross-account access |
| `TestGetJob_InstanceNotFound` | 404 when instance absent |
| `TestGetJob_MissingAuth` | 401 when auth header missing |

Total new PASS 3 tests: **16**

---

## Current limitation

- **Linux/KVM hardware validation deferred** — same constraint as M2/M3/M4.
  All tests run on the macOS dev box with in-process fake DB (memPool). No
  hardware-backed Firecracker validation performed in M5.
- The `idempotency_keys` table (schema §8, with `response_body` JSONB caching)
  is not used directly in PASS 3. Deduplication uses the existing
  `jobs.idempotency_key` unique index and sentinel jobs. Full `idempotency_keys`
  table integration with response caching is Phase 2 work.

---

## What remains for later milestones

| Work | Milestone |
|------|-----------|
| Linux/KVM hardware gate validation | M5 formal gate |
| Console UI (instance list, detail, create flow) | M6 |
| SDK / CLI client | M7 |
| Full SigV4 + KMS auth enforcement | M8 |
| `idempotency_keys` table with response body caching | Phase 2 |
| Mandatory `Idempotency-Key` enforcement on all mutating requests | Phase 2 |
| Distributed HA reconciler with lease management | Phase 2 |

---

## Commands to verify on macOS dev box

```bash
GOTOOLCHAIN=local go build ./services/resource-manager/...
GOTOOLCHAIN=local go vet ./services/resource-manager/...
GOTOOLCHAIN=local go test -v ./services/resource-manager/...
go test ./...
go build ./...
```
