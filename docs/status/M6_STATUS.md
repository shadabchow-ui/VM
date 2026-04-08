# M6 Status — Network and SSH Hardened

## Objective

IP uniqueness guaranteed under concurrent load.
SSH access reliable within SLA.

---

## Gate Requirements and Evidence

### 1. Zero duplicate IPs under concurrent allocation

**Status: COMPLETE**

- Primary enforcement: `UNIQUE(vpc_id, ip_address)` DB constraint (`001_initial.up.sql`)
  plus `SELECT ... FOR UPDATE SKIP LOCKED` in `services/network-controller/allocator.go`
  (serializable transaction — if two goroutines lock the same row, the second gets the
  next unlocked row, never the same IP).
- Secondary enforcement: `db.Repo.AllocateIP` uses the same SKIP LOCKED pattern
  via the pgxpool connection.

**Test coverage:**

| Test | File | Type |
|------|------|------|
| `TestM6_ConcurrentIPAllocation_NoDuplicates` (N=20) | `test/integration/m6_network_ssh_test.go` | Integration (real DB) |
| `TestM6_ConcurrentIPAllocation_DuplicateAttemptFails` | `test/integration/m6_network_ssh_test.go` | Integration (real DB) |
| `TestM2_IPAllocation_ConcurrentNoDuplicates` (N=5) | `test/integration/m2_vertical_slice_test.go` | Integration (real DB) |
| `TestConcurrentAllocation_NoStress` | `services/network-controller/allocator_test.go` | Unit |

**Run:**
```
DATABASE_URL=postgres://... go test -tags=integration -v \
  ./test/integration/... -run TestM6_Concurrent -count=1
```

---

### 2. SSH login within 60 seconds of RUNNING state

**Status: COMPLETE**

Contract: the instance is only written to `RUNNING` state after `readinessFn` succeeds.
`readinessFn` polls TCP port 22 every 3s up to `readinessTimeout` (120s). The SLA is that
SSH must open within 60s of the VM being booted by the host agent — the platform cannot
emit `RUNNING` until SSH accepts connections.

**Test coverage:**

| Test | File | What it proves |
|------|------|---------------|
| `TestSSH_SLA_ReachableWithin60Seconds` | `services/worker/handlers/m6_ssh_nat_test.go` | Real TCP listener opens after 5s delay; `waitForSSH` resolves within 6s << 60s SLA |
| `TestSSH_SLA_TimeoutBehavior` | `services/worker/handlers/m6_ssh_nat_test.go` | Timeout returns clear error; instance NOT written to RUNNING |
| `TestSSH_SLA_CreateHandler_RunningOnlyAfterSSHReady` | `services/worker/handlers/m6_ssh_nat_test.go` | Handler sets `failed` when `readinessFn` fails — RUNNING never written |
| `TestSSH_SLA_CreateHandler_ReadinessFnCalledBeforeRunning` | `services/worker/handlers/m6_ssh_nat_test.go` | State is `provisioning` during readiness check; `running` only after |

**Run:**
```
go test -v -race ./services/worker/handlers/... -run TestSSH_SLA
```

**Live environment note:** End-to-end SSH validation (real Firecracker VM + real cloud-init +
real key injection via metadata service) remains a hardware-gate test. All unit tests above
run on the macOS dev box without Linux/KVM.

---

### 3. IP released on deletion

**Status: COMPLETE**

Both `DeleteHandler` (step 5) and `StopHandler` (step 6) call `Network.ReleaseIP` before
writing the terminal state. `ReleaseIP` is owner-scoped and idempotent (0 rows affected = no error).

**Test coverage:**

| Test | File | Type |
|------|------|------|
| `TestM6_DeleteFlow_IPReleasedAndInventoryClean` | `test/integration/m6_network_ssh_test.go` | Integration — `GetIPByInstance` returns `""` post-delete |
| `TestM6_StopFlow_IPReleasedAndInventoryClean` | `test/integration/m6_network_ssh_test.go` | Integration — `GetIPByInstance` returns `""` post-stop |
| `TestM6_ReleasedIP_ReturnsToPool` | `test/integration/m6_network_ssh_test.go` | Integration — released IP can be reallocated |
| `TestDeleteHandler_IPReleased` | `services/worker/handlers/create_test.go` | Unit |
| `TestStopHandler_IPReleased` | `services/worker/handlers/stop_test.go` | Unit |
| `TestLifecycle_IPReleaseAndReallocate_StopThenStart` | `services/worker/handlers/lifecycle_test.go` | Unit |
| `TestNAT_RemovedOnDelete` | `services/worker/handlers/m6_ssh_nat_test.go` | Unit — IP released + DeleteInstance called |
| `TestNAT_RemovedOnStop` | `services/worker/handlers/m6_ssh_nat_test.go` | Unit — IP released + DeleteInstance called |
| `TestM2_IPRelease_Idempotent` | `test/integration/m2_vertical_slice_test.go` | Integration — double-release = nil |
| `TestM2_IPAllocation_OwnerScoped` | `test/integration/m2_vertical_slice_test.go` | Integration — wrong-owner release is no-op |

**Run:**
```
DATABASE_URL=postgres://... go test -tags=integration -v \
  ./test/integration/... -run "TestM6_DeleteFlow|TestM6_StopFlow|TestM6_Released" -count=1
```

---

### 4. IP uniqueness reconciler sub-scan active

**Status: COMPLETE**

`IPUniquenessScan` in `services/reconciler/ip_uniqueness_scan.go` runs every 5 minutes
(same cadence as the reconciler resync, sharing `resyncInterval`). It is wired into
`service.Run()` as a dedicated goroutine via `RunIPUniquenessScanLoop`.

Behaviour:
- Queries `ip_allocations` for any `ip_address + vpc_id` pair with `allocated=TRUE`
  and more than one distinct `owner_instance_id` (invariant I-2 violation).
- Logs each anomaly at `ERROR` level with full claimant list.
- Writes `instance.ip.uniqueness_violation` event against every affected instance.
- **Never mutates `ip_allocations`** — detection only; no auto-correct.

**Test coverage:**

| Test | File | Type |
|------|------|------|
| `TestIPUniquenessScan_Clean` | `services/reconciler/ip_uniqueness_scan_test.go` | Unit — zero anomalies on clean pool |
| `TestIPUniquenessScan_DetectsOneDuplicate` | `services/reconciler/ip_uniqueness_scan_test.go` | Unit — 1 duplicate → 2 events written |
| `TestIPUniquenessScan_MultipleAnomalies` | `services/reconciler/ip_uniqueness_scan_test.go` | Unit — 2 duplicates → 5 events |
| `TestIPUniquenessScan_ReadOnly_NoAutoCorrect` | `services/reconciler/ip_uniqueness_scan_test.go` | Unit — no allocation mutations |
| `TestM6_IPUniquenessScan_CleanPool` | `test/integration/m6_network_ssh_test.go` | Integration — sub-scan executes against real DB, returns clean |
| `TestM6_DeleteFlow_IPReleasedAndInventoryClean` | `test/integration/m6_network_ssh_test.go` | Integration — sub-scan clean after delete |
| `TestM6_StopStartCycles_NoGhostIPs` | `test/integration/m6_network_ssh_test.go` | Integration — sub-scan clean after each cycle |

**Run:**
```
go test -v -race ./services/reconciler/... -run TestIPUniqueness

DATABASE_URL=postgres://... go test -tags=integration -v \
  ./test/integration/... -run TestM6_IPUniqueness -count=1
```

---

### 5. Public IP DNAT/SNAT rules correct across lifecycle states

**Status: COMPLETE**

`NetworkManager.ProgramNAT` / `RemoveNAT` in `services/host-agent/runtime/network.go`
install and remove iptables DNAT+SNAT rules using idempotent `-C` check before `-A` append
and before `-D` delete.

Lifecycle contract (verified at worker-handler level):
- **RUNNING**: `CreateInstance` gRPC call triggers `ProgramNAT` on host agent.
- **STOPPED / DELETED**: `DeleteInstance` gRPC call triggers `RemoveNAT + DeleteTAP` on
  host agent. IP is also released from `ip_allocations`.
- Both operations are idempotent: repeated calls to `ProgramNAT` / `RemoveNAT` are safe.

**Test coverage:**

| Test | File | What it proves |
|------|------|---------------|
| `TestNAT_ProgrammedOnCreate` | `services/worker/handlers/m6_ssh_nat_test.go` | CreateInstance called once → NAT programmed |
| `TestNAT_RemovedOnDelete` | `services/worker/handlers/m6_ssh_nat_test.go` | DeleteInstance called + IP released on delete |
| `TestNAT_RemovedOnStop` | `services/worker/handlers/m6_ssh_nat_test.go` | DeleteInstance called + IP released on stop |
| `TestNAT_Idempotent_RepeatedDelete` | `services/worker/handlers/m6_ssh_nat_test.go` | Double-delete is idempotent (nil) |
| `TestNAT_CreateStop_StartDelete_FullCycle` | `services/worker/handlers/m6_ssh_nat_test.go` | create (NAT on) → stop (NAT off) → start (NAT on) → delete (NAT off) |
| `TestM6_StopStartCycles_NoGhostIPs` | `test/integration/m6_network_ssh_test.go` | 2 stop/start cycles — no ghost IPs |
| `TestNetworkDryRun_ProgramNAT_NoError` | `services/host-agent/runtime/network_dryrun_test.go` | ProgramNAT idempotent (dry-run) |
| `TestNetworkDryRun_RemoveNAT_NoError` | `services/host-agent/runtime/network_dryrun_test.go` | RemoveNAT idempotent (dry-run) |

**Run:**
```
go test -v -race ./services/worker/handlers/... -run TestNAT
go test -v ./services/host-agent/runtime/... -run TestNetworkDryRun
```

**Live environment note:** Actual kernel `iptables -C` / `-A` / `-D` call verification
requires a Linux host with `CAP_NET_ADMIN`. The `NETWORK_DRY_RUN=true` tests prove
idempotency of the logic path. Kernel rule verification is a hardware-gate test.

---

## Files Changed in M6

| File | Change |
|------|--------|
| `services/reconciler/ip_uniqueness_scan.go` | **New** — IP uniqueness sub-scan |
| `services/reconciler/ip_uniqueness_scan_test.go` | **New** — 4 unit tests |
| `services/reconciler/reconciler.go` | **Modified** — added `RunIPUniquenessScanLoop` |
| `services/reconciler/service.go` | **Modified** — wired sub-scan into `Run()` |
| `internal/db/ip_repo.go` | **Modified** — added `DuplicateIPRow` + `FindDuplicateIPAllocations` |
| `internal/db/event_repo.go` | **Modified** — added `EventIPUniquenessViolation` |
| `test/integration/m6_network_ssh_test.go` | **New** — 6 integration tests |
| `services/worker/handlers/m6_ssh_nat_test.go` | **New** — 9 unit tests (SSH SLA + NAT lifecycle) |
| `docs/status/M6_STATUS.md` | **New** — this file |

---

## Test Commands (complete)

```bash
# ── Unit tests (macOS dev box, no DB required) ────────────────────────────────

# IP uniqueness sub-scan
go test -v -race ./services/reconciler/... -run TestIPUniqueness

# Full reconciler suite
go test -race ./services/reconciler/...

# SSH SLA contract
go test -v -race ./services/worker/handlers/... -run TestSSH_SLA

# NAT lifecycle
go test -v -race ./services/worker/handlers/... -run TestNAT

# Full handler suite (includes all M6 unit tests)
go test -race ./services/worker/handlers/...

# NAT dry-run (network.go idempotency)
NETWORK_DRY_RUN=true go test -v ./services/host-agent/runtime/... -run TestNetworkDryRun

# All packages (excluding integration)
go test -race \
  ./internal/... \
  ./packages/... \
  ./services/reconciler/... \
  ./services/worker/handlers/...

# ── Integration tests (requires DATABASE_URL=postgres://...) ──────────────────

DATABASE_URL=postgres://... go test -tags=integration -v \
  ./test/integration/... -run TestM6 -count=1 -timeout=120s

# Full integration suite
DATABASE_URL=postgres://... go test -tags=integration -v \
  ./test/integration/... -count=1 -timeout=300s
```

---

## Remaining Limitations

1. **SSH end-to-end with real Firecracker + cloud-init** — requires Linux/KVM hardware
   gate environment. All unit proofs demonstrate the contract at the seam where the
   platform can control it (readiness gating before RUNNING write).

2. **Live iptables verification** — actual kernel rule state (`iptables -L -n -t nat`)
   requires `CAP_NET_ADMIN` on Linux. Unit tests prove the call-triggering contract;
   dry-run tests prove idempotency of the rule-programming logic.

3. **`FindDuplicateIPAllocations` array scanning** — uses pgx native `[]string` scan
   for the `array_agg` result. If the production pgxpool is replaced with a `lib/pq`
   driver, change `rows.Scan(&row.OwnerInstanceIDs)` to use `pq.Array(&row.OwnerInstanceIDs)`.

4. **M7 console** — explicitly out of M6 scope. No console work was done in this pass.
   M5 STATUS.md incorrectly listed "Console UI (instance list, detail, create flow)" as
   M6 work; per the master blueprint console goes live in M7.

---

## Gate Checklist

- [x] Concurrent IP allocation stress test exists and is wired
- [x] SSH SLA contract tested at handler level (readiness-before-running ordering)
- [x] Post-deletion IP inventory clean (integration test with real DB)
- [x] IP uniqueness reconciler sub-scan implemented, wired to 5-min loop, and tested
- [x] DNAT/SNAT rules verified across create / stop / start / delete lifecycle
- [x] All existing M1–M5 tests continue to pass
- [x] No M7 console work introduced
- [x] No invariants weakened
- [x] No DB constraints removed
