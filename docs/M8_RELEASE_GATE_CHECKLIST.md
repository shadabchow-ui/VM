# M8 Release Gate Checklist

**Milestone:** M8 — Release Ready
**Source:** `IMPLEMENTATION_PLAN_V1 §M8 exit criteria`, `master_blueprint.docx §release-readiness-checklist`
**Gate rule:** ALL items must be PASS before Phase 1 production release is authorized.

Run `make m8-gate` to execute all automatable checks.

---

## Lifecycle Matrix

| # | Requirement | Evidence Command / Test | Status |
|---|---|---|---|
| LC-1 | Create → running (happy path) | `TestCreateHandler_HappyPath_TransitionsToRunning` | ✅ PASS |
| LC-2 | Create → Stop → Start → running | `TestLifecycle_Create_Stop_Start_Delete` | ✅ PASS |
| LC-3 | Create → Reboot → running | `TestLifecycle_Create_Reboot_Delete` | ✅ PASS |
| LC-4 | Full sequence: create→stop→start→reboot→delete | `TestLifecycle_Create_Stop_Start_Reboot_Delete` | ✅ PASS |
| LC-5 | Delete from running | `TestLifecycle_Delete_FromRunning` | ✅ PASS |
| LC-6 | Delete from stopped | `TestLifecycle_Delete_FromStopped` | ✅ PASS |
| LC-7 | Delete from failed | `TestDelete_FromFailed` | ✅ PASS |
| LC-8 | All illegal transitions rejected (9×5 matrix) | `TestTransition_IllegalTransitions` (50 cases) | ✅ PASS |
| LC-9 | Illegal: stop on stopped | `TestIllegalTransition_StopOnStopped` | ✅ PASS |
| LC-10 | Illegal: start on running | `TestIllegalTransition_StartOnRunning` | ✅ PASS |
| LC-11 | Illegal: reboot on stopped | `TestIllegalTransition_RebootOnStopped` | ✅ PASS |
| LC-12 | Illegal: delete on deleting | `TestIllegalTransition_DeleteOnDeleting` | ✅ PASS |
| LC-13 | Stop failure → stays in `stopping` (retryable) | `TestLifecycle_StopFailure_RemainsInStopping` | ✅ PASS |
| LC-14 | Start failure → `failed` | `TestLifecycle_StartFailure_TransitionsToFailed` | ✅ PASS |
| LC-15 | Reboot failure → `failed` | `TestLifecycle_RebootFailure_TransitionsToFailed` | ✅ PASS |

## Failure Injection

| # | Requirement | Evidence Test | Status |
|---|---|---|---|
| FI-1 | Create fail at host selection → `failed`, no leaked resources | `TestCreateHandler_NoHosts_TransitionsToFailed` | ✅ PASS |
| FI-2 | Create fail at IP allocation → `failed` | `TestCreateHandler_IPAllocFailure_TransitionsToFailed` | ✅ PASS |
| FI-3 | Create fail at runtime → IP released, `failed` | `TestCreateHandler_CreateInstanceFailure_ReleasesIPAndFails` | ✅ PASS |
| FI-4 | Stop fail at runtime → stays in `stopping` | `TestStopHandler_StopInstanceFailure_ReturnsError` | ✅ PASS |
| FI-5 | Start fail at runtime → IP released, `failed` | `TestStartHandler_CreateInstanceFailure_ReleasesIPAndFails` | ✅ PASS |
| FI-6 | Reboot fail at stop → `failed` | `TestLifecycle_RebootFailure_TransitionsToFailed` | ✅ PASS |
| FI-7 | Failure outcomes documented for all steps | `docs/m8-failure-injection-outcomes.md` | ✅ PASS |
| FI-8 | Zero leaked resources after create rollback | IP release verified in FI-3 test above | ✅ PASS |

## Idempotency / Duplicate Delivery

| # | Requirement | Evidence Test | Status |
|---|---|---|---|
| ID-1 | Same Idempotency-Key on CREATE → original returned | `TestIdempotency_Create_SameKey` | ✅ PASS |
| ID-2 | Same key on STOP → same job_id | `TestIdempotency_Stop_SameKey` | ✅ PASS |
| ID-3 | Same key on START → same job_id | `TestIdempotency_Start_SameKey` | ✅ PASS |
| ID-4 | Same key on REBOOT → same job_id | `TestIdempotency_Reboot_SameKey` | ✅ PASS |
| ID-5 | Key reused on different instance → 409 | `TestIdempotency_Lifecycle_SameKeyDifferentInstance` | ✅ PASS |
| ID-6 | Duplicate queue delivery (re-entrant stop) → idempotent | `TestStopHandler_DuplicateDelivery_ReentrantInStopping` | ✅ PASS |
| ID-7 | Duplicate CREATE in provisioning → idempotent | `TestCreateHandler_AlreadyProvisioning_Idempotent` | ✅ PASS |
| ID-8 | DELETE on already-deleted → no-op | `TestDeleteHandler_AlreadyDeleted_IsNoOp` | ✅ PASS |

## Reconciler / Timeout

| # | Requirement | Evidence Test | Status |
|---|---|---|---|
| RT-1 | Stuck provisioning >30m → INSTANCE_CREATE repair job | `TestReconciler_StuckProvisioning_CreatesCreateJob` | ✅ PASS |
| RT-2 | Stuck starting >5m → INSTANCE_START repair job | `TestReconciler_StuckStarting_CreatesStartJob` | ✅ PASS |
| RT-3 | Stuck stopping >10m → INSTANCE_STOP repair job | `TestReconciler_StuckStopping_CreatesStopJob` | ✅ PASS |
| RT-4 | Stuck rebooting >3m → INSTANCE_REBOOT repair job | `TestReconciler_StuckRebooting_CreatesRebootJob` | ✅ PASS |
| RT-5 | Stuck deleting >15m → INSTANCE_DELETE repair job | `TestReconciler_StuckDeleting_CreatesDeleteJob` | ✅ PASS |
| RT-6 | Active job already exists → no duplicate | `TestReconciler_ActiveJobGuard_SkipsRepair` | ✅ PASS |
| RT-7 | Reconciler never mutates state directly | `TestReconciler_NoDirectStateMutation_InterfaceEnforced` | ✅ PASS |
| RT-8 | Stuck job requeued to pending by janitor | `TestJanitor_OneStuckJob_RequeuedOnce` | ✅ PASS |
| RT-9 | No stuck jobs → janitor no-op | `TestJanitor_NoStuckJobs_Noop` | ✅ PASS |
| RT-10 | Multiple stuck jobs → all requeued | `TestJanitor_MultipleStuckJobs_AllRequeued` | ✅ PASS |
| RT-11 | Concurrent update → only one wins | `TestOptimisticLock_ConcurrentUpdates_OnlyOneWins` | ✅ PASS |
| RT-12 | Stale version rejected | `TestOptimisticLock_StaleVersion_Rejected` | ✅ PASS |
| RT-13 | Wrong expected state rejected | `TestOptimisticLock_WrongExpectedState_Rejected` | ✅ PASS |
| RT-14 | Version increments after successful update | `TestOptimisticLock_VersionIncrements_AfterSuccessfulUpdate` | ✅ PASS |
| RT-15 | Job marked dead after max attempts | `TestWorkerLoop_Execute_MarksDeadWhenMaxAttemptsExhausted` | ✅ PASS |
| RT-16 | Unknown job type marked dead immediately | `TestWorkerLoop_Execute_MarksDeadForUnknownJobType` | ✅ PASS |

## Logging and Events

| # | Requirement | Evidence Test | Status |
|---|---|---|---|
| LE-1 | Create emits `instance.provisioning.start` | `TestCreateHandler_EventsWritten` | ✅ PASS |
| LE-2 | Create emits `usage.start` | `TestCreateHandler_EventsWritten` | ✅ PASS |
| LE-3 | Create emits `instance.ip.allocated` | `TestCreateHandler_EventsWritten` | ✅ PASS |
| LE-4 | Stop emits `usage.end` | `TestStopHandler_UsageEndEventWritten` | ✅ PASS |
| LE-5 | Stop emits `instance.stop.initiate` | `TestStopHandler_StopInitiateEventWritten` | ✅ PASS |
| LE-6 | Delete emits `usage.end` | `TestDeleteHandler_UsageEndEventWritten` | ✅ PASS |
| LE-7 | `usage.start` count = 2 after create+stop+start cycle | `TestLifecycle_UsageEvents_StopAndStart` | ✅ PASS |
| LE-8 | Structured logging (slog) at every handler entry | All handler `Execute` functions call `log.Info(...)` | ✅ PASS |
| LE-9 | `job_id` + `instance_id` + `attempt` in every job log | `worker/loop.go:log.With(...)` on every execute | ✅ PASS |

## Security and Secret Handling

| # | Requirement | Evidence Test | Status |
|---|---|---|---|
| SH-1 | SSH private key never in logs (create) | `TestSecretLeak_CreateHandler_HappyPath_NoPrivateKeyInLogs` | ✅ PASS |
| SH-2 | SSH private key never in logs (IP failure) | `TestSecretLeak_CreateHandler_IPFailure_NoPrivateKeyInLogs` | ✅ PASS |
| SH-3 | SSH private key never in logs (rollback) | `TestSecretLeak_CreateHandler_RuntimeFailure_NoPrivateKeyInLogs` | ✅ PASS |
| SH-4 | SSH private key never in logs (stop) | `TestSecretLeak_StopHandler_HappyPath_NoPrivateKeyInLogs` | ✅ PASS |
| SH-5 | SSH private key never in logs (stop failure) | `TestSecretLeak_StopHandler_RuntimeFailure_NoPrivateKeyInLogs` | ✅ PASS |
| SH-6 | SSH private key never in logs (start) | `TestSecretLeak_StartHandler_HappyPath_NoPrivateKeyInLogs` | ✅ PASS |
| SH-7 | SSH private key never in logs (delete) | `TestSecretLeak_DeleteHandler_HappyPath_NoPrivateKeyInLogs` | ✅ PASS |
| SH-8 | SSH private key never in logs (reboot) | `TestSecretLeak_RebootHandler_HappyPath_NoPrivateKeyInLogs` | ✅ PASS |
| SH-9 | Event payloads contain no private key material | `TestSecretLeak_EventPayloads_NoPrivateKeyInMessages` | ✅ PASS |
| SH-10 | Error values contain no private key material | `TestSecretLeak_ErrorMessages_NoPrivateKeyInErrors` | ✅ PASS |
| SH-11 | `SecretAccessKey` never appears in any log | All `TestSecretLeak_*` scan for this sentinel | ✅ PASS |
| SH-12 | SSH key API returns fingerprint only | `sshkey_handlers.go:sshKeyToResponse` (no `PublicKey` field) | ✅ PASS |
| SH-13 | Cross-account access → 404 not 403 | `TestOwnership_OtherUsersInstance` | ✅ PASS |
| SH-14 | Missing auth header → 401 | `TestAuth_MissingHeader` | ✅ PASS |
| SH-15 | Lifecycle on other user's instance → 404 | `TestOwnership_LifecycleOnOtherUsersInstance` | ✅ PASS |

## Release Artifacts

| # | Requirement | Evidence | Status |
|---|---|---|---|
| RA-1 | Failure injection outcomes documented | `docs/m8-failure-injection-outcomes.md` | ✅ PASS |
| RA-2 | Release gate checklist exists | `docs/M8_RELEASE_GATE_CHECKLIST.md` (this file) | ✅ PASS |
| RA-3 | `make m8-gate` script passes | `scripts/m8-gate-check.sh` | ✅ PASS |
| RA-4 | All unit tests pass without DB | `make test-m8` | ✅ PASS |
| RA-5 | State machine covers all 10 states × 5 actions | `TestAllStates_Count`, `TestAllActions_Count` | ✅ PASS |
| RA-6 | All binaries compile | `go build ./...` | ✅ PASS |

---

## Known Limitations (per M8 execution rules §5)

| Limitation | Mitigation |
|---|---|
| Real DB failover (primary promotion, backoff) not exercisable in unit tests | Integration tests (`test/integration/`) run against real PG; error propagation verified at code level |
| Usage event transactional atomicity (`InsertEvent` + `UpdateInstanceState` in single `BEGIN/COMMIT`) not exercisable with fakes | Integration tests verify; unit tests confirm both calls made; production `db.Pool` supports TX wrapping |
| Real Firecracker KVM lifecycle requires Linux/KVM hardware | `make m2-smoke-test` covers real hardware path; all state logic proven in unit tests |
| Reconciler has no `main.go` in this code slice | Logic fully tested in `reconciler/reconciler_test.go`; production wiring is an ops task |

---

## Sign-Off

| Role | Name | Date | Signature |
|---|---|---|---|
| Engineering Lead | ___________________ | __________ | ___________________ |
| QA Lead | ___________________ | __________ | ___________________ |
| Product Owner | ___________________ | __________ | ___________________ |
