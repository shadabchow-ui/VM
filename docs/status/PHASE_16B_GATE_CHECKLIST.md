# PHASE_16B_GATE_CHECKLIST

## VM Compute Instances Platform — Phase 16B Gate: Surface Compatibility + End-to-End Acceptance

**Status:** OPEN — not yet signed  
**Authority:** Derives from vm-16-03__blueprint__, P2_M1_GATE_CHECKLIST, and the Phase 16B bundle definition.  
**Gate Rule:** All checks must be PASS before the gate is signed. No partial sign-off.  
**Signs Off:** Engineering Lead

---

## How to Use This Checklist

For each check:
- Mark **PASS** or **FAIL** in the Status column.
- Fill in the Evidence column with a specific, linkable artifact (CI run ID, log URL, dashboard link, document reference).
- If a check FAILS, write the root cause and remediation plan before proceeding.

---

## Section 1: Prerequisites

| # | Check | Status | Evidence |
|---|-------|--------|----------|
| PRE-1 | Phase 16A (Governance + Metering) gate is signed or running in parallel with no blocking conflicts. All Phase 16A contracts (service_accounts, iam_role_bindings, usage_records, budget_policies) are frozen. | ☐ PASS / ☐ FAIL | |
| PRE-2 | P2-M1 gate was previously signed. Phase 1 lifecycle test matrix was passing in CI at the start of Phase 16. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| PRE-3 | All nine Phase 1 contracts remain FROZEN (API_ERROR_CONTRACT_V1, AUTH_OWNERSHIP_MODEL_V1, EVENTS_SCHEMA_V1, INSTANCE_MODEL_V1, IP_ALLOCATION_CONTRACT_V1, JOB_MODEL_V1, LIFECYCLE_STATE_MACHINE_V1, RUNTIMESERVICE_GRPC_V1). No DRAFT or OPEN markers. | ☐ PASS / ☐ FAIL | |

---

## Section 2: API Versioning and Compatibility (vm-16-03)

| # | Check | Status | Evidence |
|---|-------|--------|----------|
| AV-1 | `GET /healthz` returns `200 {"status":"ok"}` with a DB-connected resource manager. Returns `503` when the DB is unavailable. Response time ≤2 seconds in both cases. | ☐ PASS / ☐ FAIL | |
| AV-2 | `GET /v1/version` returns a JSON body containing `api_version` and `min_api_version` fields. No authentication required. | ☐ PASS / ☐ FAIL | |
| AV-3 | `GET /v1/openapi.json` returns an OpenAPI 3.0 document with `info.version` matching `currentAPIVersion` in the source. No authentication required. | ☐ PASS / ☐ FAIL | |
| AV-4 | All `/v1/*` responses include `X-Api-Version` response header set to the resolved API version. Verified by the acceptance integration test `TestPhase16_APIVersion_HeaderContract`. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| AV-5 | All `/v1/*` responses include `X-Request-ID` response header. The ID is a non-empty string matching the `request_id` in any error body. Source: API_ERROR_CONTRACT_V1 §7. | ☐ PASS / ☐ FAIL | |
| AV-6 | Requests without `Api-Version` header succeed and receive the current stable version in `X-Api-Version`. No `400 Bad Request` for missing header. | ☐ PASS / ☐ FAIL | |
| AV-7 | Requests using a version in `removedAPIVersions` receive `410 Gone` with structured error body containing `api_version_removed` error code. (If no removed versions exist yet, mark N/A with justification.) | ☐ PASS / N/A | |
| AV-8 | CORS `Access-Control-Allow-Headers` includes `Api-Version`. CORS `Access-Control-Expose-Headers` includes `X-Api-Version`, `X-Request-ID`, and `Location`. Browser-based console clients can read these headers. | ☐ PASS / ☐ FAIL | |

---

## Section 3: Async Operation Contract (vm-16-03)

| # | Check | Status | Evidence |
|---|-------|--------|----------|
| AS-1 | `POST /v1/instances` returns `202 Accepted` with `Location: /v1/instances/{id}/jobs/{job_id}` response header. Verified for both new and idempotent-repeat requests. | ☐ PASS / ☐ FAIL | |
| AS-2 | `POST /v1/instances/{id}/stop`, `start`, `reboot`, `DELETE /v1/instances/{id}` all return `202 Accepted` with `Location` header pointing to the enqueued job. | ☐ PASS / ☐ FAIL | |
| AS-3 | The `Location` URL in AS-1/AS-2 is pollable: `GET {Location}` returns a `Job` object with `status` field. No authentication bypass. Job must belong to the correct instance (FK enforced). | ☐ PASS / ☐ FAIL | |
| AS-4 | For idempotent-repeat lifecycle calls (same `Idempotency-Key`), the `Location` header points to the original job (same job_id), not a new one. | ☐ PASS / ☐ FAIL | |

---

## Section 4: Idempotency Contract (vm-16-03, gate Q-1)

| # | Check | Status | Evidence |
|---|-------|--------|----------|
| IDP-1 | `TestPhase16_JobIdempotency_OnConflictDoNothing` passes in CI against real PostgreSQL. Verifies that a second `InsertJob` with the same idempotency_key is silently discarded (ON CONFLICT DO NOTHING), and `GetJobByIdempotencyKey` returns the original job. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| IDP-2 | `POST /v1/instances` with `Idempotency-Key` header: duplicate request within the idempotency window returns the original instance (same ID, same 202), not a new instance. Zero duplicate instances in the DB. | ☐ PASS / ☐ FAIL | |
| IDP-3 | Lifecycle idempotency (`stop`, `start`, `reboot`, `delete`): duplicate `Idempotency-Key` on the same instance returns the original `LifecycleResponse` (same job_id). Using the same key on a different instance returns `409 idempotency_key_mismatch`. | ☐ PASS / ☐ FAIL | |

---

## Section 5: End-to-End Acceptance (Phase 16 Final Gate)

| # | Check | Status | Evidence |
|---|-------|--------|----------|
| E2E-1 | `TestPhase16_InstanceStateMachine_DBRoundTrip` passes in CI. requested → provisioning → running transitions persist with correct version increments. Stale-version CAS correctly rejected. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| E2E-2 | `TestPhase16_UsageEvents_WrittenOnStateChange` passes in CI. `usage.start` and `usage.end` events are written to the `instance_events` table and readable via `ListEvents`. Source: IMPLEMENTATION_PLAN_V1 §R-17. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| E2E-3 | `TestPhase16_Project_ScopeIsolation` passes in CI. User-scope and project-scope instance lists are strictly isolated by `owner_principal_id`. No cross-scope leakage. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| E2E-4 | Full Phase 1 lifecycle regression: `go test ./services/resource-manager/... -count=1` passes green. No previously-passing tests broken by Phase 16B changes. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| E2E-5 | `go test ./internal/db/... -count=1` passes green. All DB unit tests pass including Phase 16A IAM and metering tests. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| E2E-6 | `go test ./services/reconciler/... -count=1` passes green. Reconciler tests unaffected by Phase 16B changes. | ☐ PASS / ☐ FAIL | CI run: _________________ |
| E2E-7 | `go build ./...` succeeds with no compilation errors or unused-import warnings. | ☐ PASS / ☐ FAIL | CI run: _________________ |

---

## Section 6: Console / SDK Surface (vm-16-03)

| # | Check | Status | Evidence |
|---|-------|--------|----------|
| SDK-1 | `console/src/api_client.ts` sends `Api-Version: 2024-01-15` on every request. Verified by browser network inspector or unit test. | ☐ PASS / ☐ FAIL | |
| SDK-2 | `console/src/api_client.ts` imports types from `../index` (the canonical `types/index.ts`). No import from deprecated `../types.ts` looser shape. | ☐ PASS / ☐ FAIL | |
| SDK-3 | `instancesApi.listByProject(projectId)` is available and correctly appends `?project_id=` to the GET request. | ☐ PASS / ☐ FAIL | |
| SDK-4 | `checkAPIVersion()` function is exported and callable. Returns `compatible: true` when server version matches client. | ☐ PASS / ☐ FAIL | |
| SDK-5 | Retry policy for 429 and 5xx: client retries up to `MAX_RETRIES` times with exponential backoff + jitter. Respects `Retry-After` header when present. Does not retry 4xx errors. | ☐ PASS / ☐ FAIL | |

---

## Section 7: Operational Readiness

| # | Check | Status | Evidence |
|---|-------|--------|----------|
| OPS-1 | Kubernetes liveness probe or load balancer health check configured to call `GET /healthz`. Probe confirmed active in staging/production. | ☐ PASS / ☐ FAIL | Config reference: _________________ |
| OPS-2 | Phase 16B DB migrations (017_phase16a_iam.up.sql, 018_phase16a_metering.up.sql) applied cleanly to staging DB. Migration is idempotent — running twice does not error. | ☐ PASS / ☐ FAIL | Migration log: _________________ |
| OPS-3 | No Phase 16B change modifies `runtime.proto`, `packages/state-machine/validator.go`, or any frozen contract file. Confirm via git diff of those files showing zero changes. | ☐ PASS / ☐ FAIL | Git diff: _________________ |
| OPS-4 | `removedAPIVersions` map in `compatibility_handlers.go` is empty at Phase 16B launch (no versions have completed the 12-month deprecation period yet). Documented. | ☐ PASS / ☐ FAIL | |

---

## Sign-Off

All checks above are **PASS**:

```
Engineering Lead:  ___________________________   Date: _______________
Reviewer:          ___________________________   Date: _______________
```

**Notes / Outstanding Items:**

_Write any time-bounded follow-up items here (not blocking the gate sign-off)._
