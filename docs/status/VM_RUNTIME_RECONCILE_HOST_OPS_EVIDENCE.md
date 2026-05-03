# VM-RUNTIME-RECONCILE-HOST-OPS Evidence Note

## Job
VM-RUNTIME-RECONCILE-HOST-OPS-PHASE-D-I-K

## Date
2026-05-03

## Status
IMPLEMENTED — all scoped changes complete, all tests pass.

---

## 1. Runtime Inventory

### RuntimeInfo enrichment (vm_runtime.go)
Added fields: HostID, TapDevice, DiskPaths, SocketPath, LogPath, PolicyGen, CPUCores, MemoryMB.
Added IsRunning() and IsPresent() helpers.

### FakeRuntime enrichment (runtime_fake.go)
Create() now populates all enriched fields. HostID field added.
List() returns full inventory with all fields.

### InstanceStatus enrichment (service.go)
ListInstances exposes DataDir, TapDevice, SocketPath, LogPath, CPUCores, MemoryMB.

### Runtime-client enrichment (client.go)
InstanceStatus mirrored with full inventory fields.

## 2. Runtime-Aware Reconciliation

### Classifier (classifier.go)
New drift classes:
- DriftDBRunningNoRuntime — DB running but runtime missing
- DriftDBStoppedRuntimePresent — DB stopped/deleting but runtime present
- DriftOrphanRuntimeProcess — runtime process with no DB record
- DriftStaleHostArtifacts — deleted instance with residual artifacts

New functions:
- ClassifyRuntimeDrift() — compares DB instance with runtime inventory
- ClassifyOrphanRuntimes() — finds runtime processes with no DB instance

### Dispatcher (dispatcher.go)
All runtime-aware drift classes: detect-only — write events, NO destructive mutations.
NO repair jobs created, NO instance state changes.

## 3. Host Lifecycle Foundation

### Heartbeater (heartbeat.go)
HeartbeatLoop() now accepts VMLoadFunc to report real vm_load from runtime.
countRunningVMs() removed — replaced by injected function.

### Main.go
vmLoadFn closure wires VMRuntime.List() into heartbeater for real vm_load.

### Scheduler (placement.go)
Already excludes draining/drained/degraded/unhealthy/fenced/retired/offline/maintenance
hosts. Verified via existing tests — no changes needed.

## 4. Observability Foundation

### Event constants (event_repo.go)
New event types:
- Runtime drift: runtime.drift.db_running_no_runtime, runtime.drift.db_stopped_runtime_present,
  runtime.drift.orphan_runtime_process, runtime.drift.stale_host_artifacts
- Host lifecycle: host.drain.started, host.drain.completed, host.degraded,
  host.unhealthy, host.recovered, host.retired

## 5. Tests

### Reconciler tests
- 13 new classifier tests for runtime-aware drift
- 4 new dispatcher tests verifying detect-only behavior

### Runtime tests
- 4 new tests: InventoryShape, ListRichInventory, IsRunning, IsPresent

### Scheduler tests
- All existing tests pass — scheduler exclusion already covers all non-ready states

## 6. Detect-Only Behavior
All runtime-aware drift classes write events only. No destructive cleanup,
no auto-failure, no auto-delete, no repair jobs.

## Files Changed
- services/host-agent/runtime/vm_runtime.go
- services/host-agent/runtime/runtime_fake.go
- services/host-agent/runtime/service.go
- services/host-agent/runtime/vm_runtime_test.go
- services/host-agent/heartbeat.go
- services/host-agent/main.go
- packages/runtime-client/client.go
- services/reconciler/classifier.go
- services/reconciler/classifier_test.go
- services/reconciler/dispatcher.go
- services/reconciler/dispatcher_test.go
- internal/db/event_repo.go

## Test Results
```
services/reconciler:         PASS
services/host-agent/runtime: PASS (my tests)
services/scheduler:           PASS
services/worker/handlers:     PASS
internal/db:                  PASS
```
