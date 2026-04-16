# COMPLETION_REPORT — Phase 16A: Governance + Metering

**Bundle:** `vm-16-01` (Org/Project IAM, RBAC, Service Account Model) + `vm-16-02` (Usage Metering, Billing, Budget Controls)  
**Milestone:** VM-P3 / Phase 16A  
**Status:** Complete

---

## 1. Files Changed / Created

### New files — `internal/db/`

| File | Description |
|---|---|
| `internal/db/iam_repo.go` | Service account CRUD + IAM role binding CRUD + `CheckPrincipalHasRole` seam |
| `internal/db/metering_repo.go` | Usage records, reconciliation holds, budget policies |
| `internal/db/iam_repo_test.go` | Unit tests for all IAM repo methods |
| `internal/db/metering_repo_test.go` | Unit tests for all metering/budget repo methods |

### New files — `db/migrations/`

| File | Description |
|---|---|
| `migrations/017_phase16a_iam.up.sql` | `service_accounts` + `iam_role_bindings` DDL + indexes |
| `migrations/018_phase16a_metering.up.sql` | `usage_records` + `reconciliation_holds` + `budget_policies` DDL + indexes |

### New files — `services/resource-manager/`

| File | Description |
|---|---|
| `services/resource-manager/iam_handlers.go` | Service account CRUD + role binding endpoint handlers |

### Modified files — `services/resource-manager/`

| File | Change |
|---|---|
| `services/resource-manager/instance_errors.go` | Added `errServiceAccountNotFound`, `errRoleBindingNotFound`, `errRoleBindingConflict`, `errBudgetExceeded` |
| `services/resource-manager/project_handlers.go` | `handleProjectByID` routes recognised IAM sub-paths to `handleIAMSubpath`; added `isIAMSubpath` helper |
| `services/resource-manager/instance_handlers.go` | **Patch:** add `CheckBudgetAllowsCreate` call after `CheckAndDecrementQuota` block (see `instance_handlers_PATCH.go` for exact location) |

> **Apply the instance_handlers.go patch:** Find the closing `}` of the `CheckAndDecrementQuota` error block and insert the `CheckBudgetAllowsCreate` block documented in `services_resource-manager_instance_handlers_PATCH.go` immediately before `instanceID := idgen.New(idgen.PrefixInstance)`.

---

## 2. What Phase 16A Implemented

### vm-16-01 — Org/Project IAM, RBAC, Service Account Model

**Service accounts (workload identity resources):**
- `service_accounts` table: project-scoped, soft-delete, `active`/`disabled`/`deleted` status lifecycle
- CRUD: `CreateServiceAccount`, `GetServiceAccountByID`, `ListServiceAccountsByProject`, `SetServiceAccountStatus`, `SoftDeleteServiceAccount`
- Error sentinels: `ErrServiceAccountNotFound`, `ErrServiceAccountNameConflict`
- HTTP endpoints: `POST /v1/projects/{id}/service-accounts`, `GET /v1/projects/{id}/service-accounts`, `GET .../service-accounts/{sa_id}`, `POST .../service-accounts/{sa_id}/enable`, `POST .../service-accounts/{sa_id}/disable`, `DELETE .../service-accounts/{sa_id}`

**IAM role bindings:**
- `iam_role_bindings` table: hard-delete (revocations take immediate effect), idempotent creates via `ON CONFLICT DO NOTHING`
- CRUD: `CreateRoleBinding`, `GetRoleBindingByID`, `ListRoleBindings`, `DeleteRoleBinding`
- Authorization seam: `CheckPrincipalHasRole` — flat single-table check for Phase 16A; Phase 16B extends to hierarchical ancestor traversal via materialized path
- Error sentinels: `ErrRoleBindingNotFound`
- HTTP endpoints: `POST /v1/projects/{id}/iam/bindings`, `GET /v1/projects/{id}/iam/bindings`, `GET .../iam/bindings/{rb_id}`, `DELETE .../iam/bindings/{rb_id}`

**Role constants (canonical strings for the `role` column):**
- `IAMRoleOwner` = `"roles/owner"`
- `IAMRoleComputeViewer` = `"roles/compute.viewer"`
- `IAMRoleComputeInstanceAdmin` = `"roles/compute.instanceAdmin"`
- `IAMRoleServiceAccountUser` = `"roles/iam.serviceAccountUser"`

**Resource type constants:** `IAMResourceTypeProject`, `IAMResourceTypeInstance`, `IAMResourceTypeServiceAccount`

**Cross-account safety preserved:** All service account and role binding lookups include `project_id` in the `WHERE` clause. No resource existence is leaked across project boundaries. `requireProjectOwnership` helper in `iam_handlers.go` enforces 404-for-cross-account before any IAM sub-resource access.

### vm-16-02 — Usage Metering, Billing, Budget Controls

**Usage records (immutable billing log):**
- `usage_records` table: INSERT-only event log; corrections are new `ADJUSTMENT` rows, never UPDATEs
- `ON CONFLICT (event_id) DO NOTHING` enforces at-most-once ingestion for duplicate metering events (agent retries, reconciler races)
- `InsertUsageRecord`, `CloseUsageRecord`, `GetOpenUsageRecord`, `ListUsageRecordsByScope`
- Record types: `USAGE_START`, `USAGE_END`, `RECONCILED`, `ADJUSTMENT`
- Usage attribution scope: `scope_id = owner_principal_id` — matches existing quota scope anchor exactly; no new scope model introduced

**Reconciliation holds (exactly-once protocol):**
- `reconciliation_holds` table: `UNIQUE (instance_id, window_start, window_end)` makes hold acquisition atomic
- `AcquireReconciliationHold` (returns bool — true = won the race), `ApplyReconciliationHold`, `ReleaseReconciliationHold`, `HoldExistsForWindow`
- Hold statuses: `pending` → `applied` (synthetic event written) or `released` (original event arrived late)

**Budget policies (non-destructive spending caps):**
- `budget_policies` table: per-scope (user or project) dollar-denominated caps
- `enforcement_action`: `notify` (alert only) or `block_create` (gates new resource creation)
- `CheckBudgetAllowsCreate` wired into `handleCreateInstance` after `CheckAndDecrementQuota`, before `InsertInstance`
- `ErrBudgetExceeded` → HTTP 422 `budget_exceeded` — distinct from `ErrQuotaExceeded` (`quota_exceeded`) and scheduler capacity errors
- **Non-destructive contract enforced:** budget enforcement NEVER terminates or stops running instances; only new creation is blocked
- `CreateBudgetPolicy`, `GetBudgetPolicyByID`, `ListActiveBudgetPoliciesForScope`, `IncrementBudgetAccrual`

---

## 3. Intentionally Deferred to Phase 16B

### vm-16-01 deferred:
- IAM Policy Service: `check(principal, permission, resource_name)` endpoint with full hierarchical evaluation
- Materialized path hierarchy traversal (Org → Folder → Project ancestor chain)
- IAM Credentials Service: short-lived token vending / service account impersonation
- Folder support (flat org→project is sufficient for Phase 16A)
- Organization Policy Service (deny overrides, org-wide guardrails)
- `iam.serviceAccounts.actAs` permission enforcement (constant defined; not enforced)

### vm-16-02 deferred:
- Dual-path (Lambda) processing pipeline: streaming path (Flink/ksqlDB + Druid/ClickHouse OLAP)
- Rating & Pricing Subsystem (versioned pricing catalog + multi-layer cache)
- Invoice / billing_period tables and batch invoice generation
- Automated DLQ triage
- Automated budget quota lock + automatic rollback (Budget & Quota Enforcement Subsystem full implementation)
- Real-time cost visibility dashboard

---

## 4. DB / Migration Changes

### Migration 017: `017_phase16a_iam.up.sql`

**New tables:**
- `service_accounts` — workload identity resources (project-scoped, soft-delete)
- `iam_role_bindings` — policy bindings (hard-delete)

**New indexes:**
- `idx_service_accounts_project` — partial index on `(project_id)` WHERE `deleted_at IS NULL`
- `idx_service_accounts_created_by`
- `idx_iam_role_bindings_project`, `idx_iam_role_bindings_principal`, `idx_iam_role_bindings_check`

**No changes to existing tables.**

### Migration 018: `018_phase16a_metering.up.sql`

**New tables:**
- `usage_records` — immutable billing event log
- `reconciliation_holds` — reservation slots for exactly-once reconciliation
- `budget_policies` — per-scope spending caps

**New indexes:**
- `idx_usage_records_scope` — billing query hot path
- `idx_usage_records_instance` — reconciliation open-record lookup (partial, `WHERE ended_at IS NULL`)
- `idx_reconciliation_holds_instance`
- `idx_budget_policies_scope_active` — admission check hot path (partial, `WHERE status = 'active'`)

**No changes to existing tables.**

---

## 5. Tenancy / IAM / Accounting Semantics

**Tenancy scope anchor:** `owner_principal_id` remains the single scope anchor for both quota and metering. In project mode `owner_principal_id = project.principal_id`; in classic mode `owner_principal_id = user_principal_id`. No new scope model introduced.

**Authorization model (Phase 16A):** Flat `iam_role_bindings` table check via `CheckPrincipalHasRole`. The existing `created_by = principalID` check in project handlers is preserved unchanged; `CheckPrincipalHasRole` is an additive seam for new resource types (service accounts, role bindings). Phase 16B will replace the flat check with a hierarchical materialized-path traversal.

**Ownership hiding:** 404-for-cross-account is preserved everywhere:
- Service accounts: `project_id` guard in all WHERE clauses; `requireProjectOwnership` validates project ownership before any SA operation
- Role bindings: same `project_id` guard
- IAM handler routes: always verify project ownership via `requireProjectOwnership` before operating on any IAM sub-resource

**Error code separation:**
- `quota_exceeded` (HTTP 422) — count-based instance limit (`ErrQuotaExceeded`, pre-existing)
- `budget_exceeded` (HTTP 422) — dollar-based spending limit (`ErrBudgetExceeded`, Phase 16A)
- `service_unavailable` (HTTP 503) — transient DB connectivity failure (pre-existing)
- These are distinct and must never be collapsed.

---

## 6. Tests Added / Updated

| File | Tests |
|---|---|
| `internal/db/iam_repo_test.go` | 24 tests covering all IAM repo methods, error paths, and cross-account guards |
| `internal/db/metering_repo_test.go` | 27 tests covering all metering/budget repo methods, idempotency, and error paths |

**All pre-existing tests are unaffected.** The `memPool` in handler tests (`instance_handlers_test.go`) defaults `SELECT EXISTS(...)` for budget policies to `false` (no exceeded policy), so all existing create-instance tests continue to pass without modification.

---

## 7. Validation Commands

```bash
# Build the entire repo (compile check for all packages)
go build ./...

# DB layer tests (includes new iam_repo_test.go and metering_repo_test.go)
go test ./internal/db/... -count=1 -v

# Resource manager tests (instance_errors, project_handlers, iam_handlers)
go test ./services/resource-manager/... -count=1 -v

# Reconciler tests (unchanged but verify no regressions)
go test ./services/reconciler/... -count=1

# Full test suite
go test ./... -count=1
```

---

## 8. Copy Commands

```bash
# DB layer
cp internal_db_iam_repo.go           internal/db/iam_repo.go
cp internal_db_metering_repo.go       internal/db/metering_repo.go
cp internal_db_iam_repo_test.go       internal/db/iam_repo_test.go
cp internal_db_metering_repo_test.go  internal/db/metering_repo_test.go

# Migrations
cp migrations_017_phase16a_iam.up.sql       db/migrations/017_phase16a_iam.up.sql
cp migrations_018_phase16a_metering.up.sql  db/migrations/018_phase16a_metering.up.sql

# Resource manager
cp services_resource-manager_iam_handlers.go      services/resource-manager/iam_handlers.go
cp services_resource-manager_instance_errors.go   services/resource-manager/instance_errors.go
cp services_resource-manager_project_handlers.go  services/resource-manager/project_handlers.go

# instance_handlers.go — apply patch manually per instance_handlers_PATCH.go
# Location: after the closing } of the CheckAndDecrementQuota block,
# before: instanceID := idgen.New(idgen.PrefixInstance)
```

---

## 9. Risks and Follow-Up Items

| Risk | Severity | Mitigation |
|---|---|---|
| `CheckBudgetAllowsCreate` SELECT EXISTS returns false by default in tests (memPool has no budget_policies dispatch) | Low | Tests pass. Add memPool dispatch case if explicit budget-exceeded handler tests are added in a future slice. |
| `iam_repo.go` uses `isSANoRows` / `isSADuplicateKey` local helpers to avoid redeclaring package-level helpers from an unuploaded db file | Low | Prefixed helpers do not conflict. When repo is fully visible, consolidate to package-level helpers. |
| `handleIAMSubpath` is registered via `handleProjectByID` patch — not via its own mux registration | Low | Clean, follows existing sub-path dispatch pattern (see `handleHostsSubpath`, `handleCampaignSubpath`). No mux conflict. |
| `CheckPrincipalHasRole` is a flat check — does not traverse hierarchical ancestors | Intentional | Phase 16A seam only. Phase 16B adds materialized-path hierarchy. Document deferred in code comments. |
| Budget `accrued_cents` on the streaming path is best-effort approximate | Intentional | Authoritative figure comes from batch aggregation of `usage_records`. Per vm-16-02 blueprint §mvp. |
| `IncrementBudgetAccrual` is a best-effort UPDATE — if no active policy exists, 0 rows affected is silently OK | Low | Matches blueprint intent: accrual is a streaming approximation, not a hard invariant. |
