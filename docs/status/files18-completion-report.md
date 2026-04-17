# COMPLETION_REPORT.md — VM-P3C: Rollout Controls + Operational Polish

**Bundle:** VM-P3C  
**Scope:** Rollout controls for VM subsystems + final operational polish

---

## 1. Files Changed / Created

| Path | Action | Description |
|------|--------|-------------|
| `services/reconciler/rollout_gate.go` | NEW | `RolloutGate` — atomic pause/resume for repair dispatch |
| `services/reconciler/rollout_gate_test.go` | NEW | 9 unit tests: lifecycle, idempotency, race safety |
| `services/reconciler/dispatcher.go` | PATCHED | Adds `SetGate()` + gate check in `enqueueRepairJob()` |
| `services/resource-manager/rollout_handlers.go` | NEW | Pause/resume/status admin endpoints |
| `services/resource-manager/rollout_handlers_test.go` | NEW | 17 unit tests for all three endpoints |
| `services/resource-manager/main.go` | PATCHED | Adds `rolloutGate RolloutGateInterface` to server struct |
| `services/resource-manager/api_p3c_patch.go` | NEW | Patch instructions for api.go (delete after applying) |
| `test/integration/p3c_acceptance_test.go` | NEW | P3C acceptance gate (10 tests, tagged integration) |

---

## 2. What VM-P3C Implements

### RolloutGate (services/reconciler/rollout_gate.go)
Lock-free atomic pause/resume for repair dispatch. Pausing does NOT stop drift detection, does NOT terminate VMs, does NOT cancel in-flight jobs. failInstance() is explicitly NOT gated — state corrections safe during rollouts.

### Dispatcher patch (services/reconciler/dispatcher.go)
- `SetGate(gate *RolloutGate)` method added
- Gate checked in `enqueueRepairJob()` only — not in `failInstance()`
- nil gate → no change to existing behavior (backward compatible)

### Admin endpoints (services/resource-manager/rollout_handlers.go)
All mTLS-protected via auth.RequireMTLS:
- POST /internal/v1/rollout/pause   — requires {"reason":"..."}, idempotent
- POST /internal/v1/rollout/resume  — no body, idempotent  
- GET  /internal/v1/rollout/status  — non-mutating

Uses local RolloutGateInterface (not reconciler package import) to avoid circular imports.

### main.go patch
Added rolloutGate RolloutGateInterface field to server struct. Wire-up documented in comment; left nil by default for backward compat.

---

## 3. Required Manual Patch: api.go

In routes(), after `s.registerProjectRoutes(mux)`, add:

    // VM-P3C: Rollout control admin endpoints.
    s.registerRolloutRoutes(mux)

Then delete api_p3c_patch.go.

---

## 4. DB / Migration Changes

None. Gate is entirely in-memory.

---

## 5. Copy Commands

    cp p3c/services/reconciler/rollout_gate.go      services/reconciler/rollout_gate.go
    cp p3c/services/reconciler/rollout_gate_test.go services/reconciler/rollout_gate_test.go
    cp p3c/services/reconciler/dispatcher.go        services/reconciler/dispatcher.go
    cp p3c/services/resource-manager/rollout_handlers.go     services/resource-manager/rollout_handlers.go
    cp p3c/services/resource-manager/rollout_handlers_test.go services/resource-manager/rollout_handlers_test.go
    cp p3c/services/resource-manager/main.go        services/resource-manager/main.go
    cp p3c/test/integration/p3c_acceptance_test.go  test/integration/p3c_acceptance_test.go
    # Apply api.go patch manually (see §3), then: rm api_p3c_patch.go

---

## 6. Test Commands

    go build ./...
    go test ./services/reconciler/... -count=1 -v
    go test ./services/resource-manager/... -count=1 -v
    go test -race ./services/reconciler/... -count=1
    go test -tags=integration -v ./test/integration/... -run TestP3C
    DATABASE_URL=postgres://... go test -tags=integration -v ./test/integration/... -run TestP3C_DB
    go test ./... -count=1

---

## 7. Environment-Blocked Checks

TestP3C_DB_JobNotWritten_WhenGatePaused requires DATABASE_URL — not a code failure.

---

## 8. Risks / Follow-up

- api_p3c_patch.go must be deleted after applying the api.go change
- Multi-process deployment: in-memory gate does not propagate across processes — both resource-manager and reconciler must each hold the gate; shared-gate store (DB flag / Redis) deferred to Phase 2
- RolloutGateStatus defined locally in resource-manager — if reconciler package exports it later, update to use the canonical type
