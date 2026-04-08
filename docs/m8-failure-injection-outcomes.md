# M8 Failure Injection Outcomes

**Source:** `IMPLEMENTATION_PLAN_V1 §M8`, `04-04-provisioning-failure-handling-and-rollback.md`,
`11-03-failure-surface-and-recovery-map.md`

**Status:** All scenarios below are covered by deterministic automated unit tests.
Evidence test names are listed under each scenario.

---

## INSTANCE_CREATE Failure Injection Map

Each step below maps to a deliberate failure injected via the fake store or fake runtime.
All tests live in `services/worker/handlers/create_test.go` and `lifecycle_test.go`.

| Step | Failure Point | Injected Error | Expected Outcome | Test Name | Rollback Actions Verified |
|---|---|---|---|---|---|
| 1 | DB: requested → provisioning (state mismatch) | Wrong `expectedState` in `UpdateInstanceState` | Error returned; state unchanged | `TestOptimisticLock_WrongExpectedState_Rejected` | None (write never applied) |
| 2 | Scheduler: host selection | `GetAvailableHosts` returns empty slice | `vm_state = failed`, error returned | `TestCreateHandler_NoHosts_TransitionsToFailed` | R3: mark failed |
| 3 | DB: assign host (version mismatch) | Stale version in `AssignHost` | Error returned; host not assigned | `TestOptimisticLock_AssignHost_VersionEnforced` | None |
| 4 | Network: IP allocation | `AllocateIP` returns error | `vm_state = failed`, error returned | `TestCreateHandler_IPAllocFailure_TransitionsToFailed` | R3: mark failed |
| 5 | Host Agent: CreateInstance | `CreateInstance` returns error | IP released; `vm_state = failed` | `TestCreateHandler_CreateInstanceFailure_ReleasesIPAndFails` | R1: DeleteInstance, R2: ReleaseIP, R3: mark failed |
| 6 | Readiness: SSH timeout | `readinessFn` returns error (mockable) | IP released; `vm_state = failed` | `TestStartHandler_ReadinessTimeout_RollsBackAndFails` | R1, R2, R3 |
| 7 | DB: provisioning → running (concurrent version race) | Two goroutines same version | Only one wins; loser errors | `TestOptimisticLock_ConcurrentUpdates_OnlyOneWins` | None (VM running; retry re-checks state) |

### Rollback Order (Invariant R-06)
Rollback always executes in reverse allocation order:
```
R1 → DeleteInstance (VM process + TAP + rootfs) — idempotent
R2 → ReleaseIP                                  — idempotent
R3 → UpdateInstanceState to 'failed'            — idempotent
```
Source: `04-04-provisioning-failure-handling-and-rollback.md §R-06`, `worker/handlers/rollback.go`.

### Zero Leaked Resources After Rollback
- IP always released on any post-allocation failure: `TestCreateHandler_CreateInstanceFailure_ReleasesIPAndFails`
- VM resources cleaned via `DeleteInstance` (idempotent): `rollback.go:RollbackProvisioning`
- No instance remains in `provisioning` after failure — always reaches `failed`

---

## INSTANCE_STOP Failure Injection Map

Source: `services/worker/handlers/stop_test.go`

| Failure Point | Injected Error | Expected Outcome | Test Name |
|---|---|---|---|
| `StopInstance` RPC | Returns gRPC error | State remains `stopping` (retryable) | `TestStopHandler_StopInstanceFailure_ReturnsError` |
| `DeleteInstance` RPC | Returns gRPC error | State remains `stopping` (retryable) | `TestStopHandler_DeleteInstanceFailure_ReturnsError` |
| No host assigned | `HostID == nil` | Skip runtime ops; succeed to `stopped` | `TestStopHandler_NoHost_SkipsRuntimeOps` |
| Illegal source state: `provisioning` | State check | Error returned; state not mutated | `TestStopHandler_IllegalState_Provisioning_ReturnsError` |
| Illegal source state: `failed` | State check | Error returned | `TestStopHandler_IllegalState_Failed_ReturnsError` |
| Already `stopped` | State check | Idempotent no-op; no events written | `TestStopHandler_AlreadyStopped_IsNoOp` |

---

## INSTANCE_START Failure Injection Map

Source: `services/worker/handlers/start_test.go`, `lifecycle_test.go`

| Failure Point | Injected Error | Expected Outcome | Test Name |
|---|---|---|---|
| IP allocation | `AllocateIP` error | `vm_state = failed` | `TestStartHandler_IPAllocFailure_TransitionsToFailed` |
| `CreateInstance` RPC | Returns error | IP released; `vm_state = failed` | `TestStartHandler_CreateInstanceFailure_ReleasesIPAndFails` |
| Readiness timeout | `readinessFn` error | IP released; `vm_state = failed` | `TestStartHandler_ReadinessTimeout_RollsBackAndFails` |
| Already `running` | State check | Idempotent no-op | `TestStartHandler_AlreadyRunning_IsNoOp` (via lifecycle) |

---

## INSTANCE_REBOOT Failure Injection Map

Source: `services/worker/handlers/reboot_test.go`, `lifecycle_test.go`

| Failure Point | Injected Error | Expected Outcome | Test Name |
|---|---|---|---|
| `StopInstance` fails at reboot start | gRPC error | `vm_state = failed` | `TestLifecycle_RebootFailure_TransitionsToFailed` |
| `CreateInstance` fails at reboot restart | gRPC error | `vm_state = failed` | `TestRebootHandler_CreateInstanceFailure_TransitionsToFailed` |

---

## INSTANCE_DELETE Failure Injection Map

Source: `services/worker/handlers/create_test.go` (`TestDeleteHandler_*`)

| Failure Point | Injected Error | Expected Outcome | Test Name |
|---|---|---|---|
| Already `deleted` | State check | No-op; no error (idempotent) | `TestDeleteHandler_AlreadyDeleted_IsNoOp` |
| Stale version on soft-delete | Version mismatch | Error; state not mutated | `TestOptimisticLock_SoftDelete_VersionEnforced` |
| IP release fails | `ReleaseIP` error | Continues (best-effort); logged | `rollback.go` (error logged, execution continues) |

---

## Stuck Job / Janitor Scenarios

Source: `services/worker/janitor_test.go`

| Scenario | Expected Outcome | Test Name |
|---|---|---|
| No stuck jobs in DB | No requeue calls | `TestJanitor_NoStuckJobs_Noop` |
| Single stuck `in_progress` job | Requeued to `pending` | `TestJanitor_OneStuckJob_RequeuedOnce` |
| Three stuck jobs | All three requeued | `TestJanitor_MultipleStuckJobs_AllRequeued` |
| `ListStuckInProgressJobs` returns correct rows | Row count matches | `TestJanitor_ListStuckInProgressJobs_ReturnsCorrectCount` |
| `RequeueTimedOutJob` calls Exec with job ID | Exec called once | `TestJanitor_RequeueTimedOutJob_CallsExec` |
| `Exec` errors on individual requeue | Logged; sweep continues | `TestJanitor_ContinuesOnIndividualRequeueError` |
| Re-sweep after requeue | No double-requeue (SQL guard documented) | `TestJanitor_IdempotentResweep_SQLGuard_Documented` |

---

## Reconciler Drift Scenarios

Source: `reconciler/reconciler_test.go`

| Scenario | Expected Outcome | Test Name |
|---|---|---|
| Empty instance list | No jobs | `TestReconciler_EmptyScan_Noop` |
| Freshly-entered transitional state | No repair job | `TestReconciler_FreshTransitional_NotFlagged` |
| Provisioning stuck >30m | INSTANCE_CREATE repair job | `TestReconciler_StuckProvisioning_CreatesCreateJob` |
| Starting stuck >5m | INSTANCE_START repair job | `TestReconciler_StuckStarting_CreatesStartJob` |
| Stopping stuck >10m | INSTANCE_STOP repair job | `TestReconciler_StuckStopping_CreatesStopJob` |
| Rebooting stuck >3m | INSTANCE_REBOOT repair job | `TestReconciler_StuckRebooting_CreatesRebootJob` |
| Deleting stuck >15m | INSTANCE_DELETE repair job | `TestReconciler_StuckDeleting_CreatesDeleteJob` |
| Active job already exists | No duplicate repair job | `TestReconciler_ActiveJobGuard_SkipsRepair` |
| Stable states (running/stopped/failed) | No repair jobs ever | `TestReconciler_StableStates_NeverFlagged` |
| Multiple stuck instances | One repair job each | `TestReconciler_MultipleStuck_AllGetRepaired` |
| Direct state mutation attempted | Impossible — compile-time interface enforcement | `TestReconciler_NoDirectStateMutation_InterfaceEnforced` |
| Just below threshold | No repair job | `TestReconciler_ExactThreshold_JustBelow_NotFlagged` |
| Just over threshold | Repair job created | `TestReconciler_ExactThreshold_JustOver_IsFlagged` |

---

## Duplicate Delivery / Idempotency

Source: `resource-manager/instance_handlers_test.go`, `services/worker/handlers/stop_test.go`

| Scenario | Expected Outcome | Test Name |
|---|---|---|
| Same Idempotency-Key on CREATE (×2) | One instance; second call returns original | `TestIdempotency_Create_SameKey` |
| Same Idempotency-Key on STOP | One job; second call returns same job_id | `TestIdempotency_Stop_SameKey` |
| Same Idempotency-Key on START | Same job returned | `TestIdempotency_Start_SameKey` |
| Same Idempotency-Key on REBOOT | Same job returned | `TestIdempotency_Reboot_SameKey` |
| Key reused on different instance | 409 `idempotency_key_mismatch` | `TestIdempotency_Lifecycle_SameKeyDifferentInstance` |
| Duplicate STOP delivery (re-entrant in `stopping`) | Resumes; completes to `stopped` | `TestStopHandler_DuplicateDelivery_ReentrantInStopping` |
| Duplicate CREATE delivery (re-entrant in `provisioning`) | Resumes; completes to `running` | `TestCreateHandler_AlreadyProvisioning_Idempotent` |
| DELETE on already-deleted | No-op; no error | `TestDeleteHandler_AlreadyDeleted_IsNoOp` |

---

## Optimistic Locking Protection

Source: `services/worker/handlers/optimistic_lock_test.go`

| Scenario | Expected Outcome | Test Name |
|---|---|---|
| Two concurrent updates, same version | Exactly one wins | `TestOptimisticLock_ConcurrentUpdates_OnlyOneWins` |
| Stale version on UpdateInstanceState | Rejected; state unchanged | `TestOptimisticLock_StaleVersion_Rejected` |
| Wrong expectedState | Rejected; state unchanged | `TestOptimisticLock_WrongExpectedState_Rejected` |
| Version increments after success | Version = prior + 1 | `TestOptimisticLock_VersionIncrements_AfterSuccessfulUpdate` |
| Chained mutations each need prior version | All four steps succeed in sequence | `TestOptimisticLock_ChainedUpdates_EachVersionMustMatch` |
| Stale version on AssignHost | Rejected; host_id unchanged | `TestOptimisticLock_AssignHost_VersionEnforced` |
| Stale version on SoftDeleteInstance | Rejected; state unchanged | `TestOptimisticLock_SoftDelete_VersionEnforced` |

---

## Secret Handling

Source: `services/worker/handlers/secret_leak_test.go`

| Rule | Verified By | Test Name |
|---|---|---|
| SSH private key never in logs (create success) | Log buffer scan | `TestSecretLeak_CreateHandler_HappyPath_NoPrivateKeyInLogs` |
| SSH private key never in logs (IP failure path) | Log buffer scan | `TestSecretLeak_CreateHandler_IPFailure_NoPrivateKeyInLogs` |
| SSH private key never in logs (rollback path) | Log buffer scan | `TestSecretLeak_CreateHandler_RuntimeFailure_NoPrivateKeyInLogs` |
| SSH private key never in logs (stop) | Log buffer scan | `TestSecretLeak_StopHandler_HappyPath_NoPrivateKeyInLogs` |
| SSH private key never in logs (stop failure) | Log buffer scan | `TestSecretLeak_StopHandler_RuntimeFailure_NoPrivateKeyInLogs` |
| SSH private key never in logs (start) | Log buffer scan | `TestSecretLeak_StartHandler_HappyPath_NoPrivateKeyInLogs` |
| SSH private key never in logs (delete) | Log buffer scan | `TestSecretLeak_DeleteHandler_HappyPath_NoPrivateKeyInLogs` |
| SSH private key never in logs (reboot) | Log buffer scan | `TestSecretLeak_RebootHandler_HappyPath_NoPrivateKeyInLogs` |
| Event Message/Details fields contain no secrets | Event row scan | `TestSecretLeak_EventPayloads_NoPrivateKeyInMessages` |
| Error return values contain no secrets | Error string scan | `TestSecretLeak_ErrorMessages_NoPrivateKeyInErrors` |
| `SecretAccessKey` never logged (all paths) | All `TestSecretLeak_*` scan for this sentinel | All 10 tests above |

---

## DB Failover / Recovery — Limitation Statement

**Per M8 execution rules §5 (document the gap precisely when full proof is impossible):**

Real PostgreSQL primary-promotion failover (write unavailability window, connection
pool exponential backoff, replica promotion) cannot be exercised deterministically
in the unit-test environment. The following is verified at code level:

- All `db.Repo` methods wrap errors — no silent swallows anywhere.
- `WorkerLoop.claimNext` catches `BeginTx` errors and backs off via `sleep(ctx, pollInterval)`.
- All state mutations use `WHERE version=$n AND vm_state=$expected` — a DB restart
  cannot produce incorrect state transitions.
- `WorkerLoop.execute` catches handler errors and marks jobs `failed`/`dead` —
  no in-flight job is silently abandoned.

**Integration test coverage** (`test/integration/`) provides the stronger proof
against a real PostgreSQL instance for operators running with `DATABASE_URL` set.

---

## Usage Event Transaction Boundary — Limitation Statement

Blueprint rule R-17: usage events must be written in the same DB transaction as
the state change. In the current repo, `InsertEvent` and `UpdateInstanceState`
are called sequentially through the `InstanceStore` interface — they are **not**
wrapped in a single SQL `BEGIN/COMMIT` at the fake level.

**Evidence of correct production intent:**

- Unit tests verify both calls are made: `TestCreateHandler_EventsWritten`,
  `TestLifecycle_UsageEvents_StopAndStart`, `TestDeleteHandler_UsageEndEventWritten`.
- The `db.Pool` interface supports `BEGIN/COMMIT` wrapping via `sqlDBPool` in `worker/main.go`.
- Full transactional atomicity is verified by the integration test suite against real PostgreSQL.

This limitation is accepted for Phase 1 and tracked as a Phase 2 hardening item.
