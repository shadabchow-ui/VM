# REAL_RUNTIME_GAP_MAP.md — Runtime Gap Map

> Generated: 2026-05-02 | Branch: main | Repo: shadabchow-ui/VM

This document maps every runtime seam in the repo, labels what is real vs mock/fake/dry-run,
and identifies the exact gaps to close before the control plane can boot real guests on real
hardware.

---

## 1. Worker Handler Lifecycle Assumptions

### 1.1 INSTANCE_CREATE (`services/worker/handlers/create.go`)

| Step | Operation | Mechanism | Real / Mock |
|------|-----------|-----------|-------------|
| 1 | DB: requested → provisioning | `UpdateInstanceState` with optimistic lock | **Real** — uses `db.Repo` via `InstanceStore` interface |
| 2 | Host selection | `GetAvailableHosts`, first-fit by free CPU | **Real** — calls DB |
| 3 | Host assignment | `AssignHost` with version guard | **Real** — calls DB |
| 3b | Root disk record | `CreateRootDisk`, idempotent on `GetRootDiskByInstanceID` | **Real** — calls DB |
| 4 | IP allocation | `Network.AllocateIP` → `SELECT FOR UPDATE SKIP LOCKED` | **Real** — uses `NetworkController` interface (production: `NetworkControllerClient`) |
| 5 | Host Agent: CreateInstance | `RuntimeClient.CreateInstance` via runtimeFactory | **Real call path** — production hits `runtimeclient.Client` (HTTP/JSON to host-agent) |
| 6 | SSH readiness | `waitForSSH` — TCP dial to port 22, 3s poll, 120s timeout | **Escape hatch**: `READINESS_DRY_RUN=true` skips entirely |
| 7 | DB: provisioning → running | `UpdateInstanceState` + `UpdateRootDiskStatus` + optional NIC status | **Real** — calls DB |

**Key assumption**: The worker assumes the Host Agent responds to HTTP/JSON at `<hostID>:50051`.
Step 5 is a real call; Step 6 can be disabled via env var.

### 1.2 INSTANCE_START (`services/worker/handlers/start.go`)

| Step | Mechanism | Real / Mock |
|------|-----------|-------------|
| 1-2 | Load + transition stopped → provisioning | **Real** — DB operations |
| 3-4 | Host selection + assignment | **Real** — DB operations |
| 5 | IP retrieval (retained from create) or fallback allocation | **Real** — `GetIPByInstance` + optional `AllocateIP` |
| 6 | Host Agent: CreateInstance (full re-provision) | **Real call path** |
| 7 | SSH readiness | **Escape hatch**: `READINESS_DRY_RUN=true` |
| 8 | DB: provisioning → running + NIC re-attach | **Real** — DB operations |

**Key assumption**: Phase 1 start = full re-provision (stop releases all runtime resources).
IP is retained across stop/start cycles per `IP_ALLOCATION_CONTRACT_V1 §5`.

### 1.3 INSTANCE_STOP (`services/worker/handlers/stop.go`)

| Step | Mechanism | Real / Mock |
|------|-----------|-------------|
| 1-3 | Load + transition running → stopping | **Real** — DB operations |
| 4 | Host Agent: StopInstance (ACPI + force) | **Real call path** |
| 5 | Host Agent: DeleteInstance (releases TAP + rootfs) | **Real call path** |
| 6 | NIC status → detached (IP retained) | **Real** — DB operations |
| 7 | DB: stopping → stopped + usage.end event | **Real** — DB operations |

**Key assumption**: Stop = full teardown on host (TAP destroyed, VM process killed, rootfs released).
IP is NOT released — it's retained for restart.

### 1.4 INSTANCE_REBOOT (`services/worker/handlers/reboot.go`)

| Step | Mechanism | Real / Mock |
|------|-----------|-------------|
| 1-3 | Load + transition running → rebooting | **Real** — DB operations |
| 4 | Host Agent: StopInstance (same host) | **Real call path** |
| 5 | Host Agent: CreateInstance (same host, same rootfs, same IP) | **Real call path** |
| 6 | SSH readiness | **Escape hatch**: `READINESS_DRY_RUN=true` |
| 7 | DB: rebooting → running | **Real** — DB operations |

**Key assumption**: Reboot stays on the same host. DeleteInstance is NOT called — rootfs preserved.
Requires host assignment (`inst.HostID != nil`).

### 1.5 INSTANCE_DELETE (`services/worker/handlers/delete.go`)

| Step | Mechanism | Real / Mock |
|------|-----------|-------------|
| 1-2 | Load + transition → deleting | **Real** — DB operations |
| 3 | Root disk disposition (delete_on_termination) | **Real** — `GetRootDiskByInstanceID` |
| 4-5 | Host Agent: StopInstance + DeleteInstance | **Real call path** |
| 6 | DB: DeleteRootDisk or DetachRootDisk | **Real** — DB operations |
| 7 | IP release | **Real** — `Network.ReleaseIP` |
| 7b | NIC soft-delete | **Real** — DB operations |
| 8 | SoftDeleteInstance | **Real** — DB operations |

---

## 2. Runtime Client Transport

### 2.1 Current Transport

| File | Transport | Target | Auth |
|------|-----------|--------|------|
| `packages/runtime-client/client.go` | **HTTP/JSON** | `http://<hostID>:50051` | **None** (plaintext internal) |

The client implements: `CreateInstance`, `StopInstance`, `DeleteInstance`, `ListInstances`.
It sends JSON POST/GET to the Host Agent's HTTP server.

**Source comment** (`client.go:8-13`): "The proto contract specifies gRPC transport. For M2, we implement an HTTP/JSON client... Replacing with gRPC is a pure transport swap — no caller changes required."

### 2.2 Intended Final Transport

| Component | Current | Intended | Gap |
|-----------|---------|----------|-----|
| Protocol | HTTP/JSON | **gRPC** | `packages/contracts/runtimev1/runtime.proto` exists; no protoc generation done |
| Auth | None (plaintext) | **mTLS** with host-specific certs (CN=host-{host_id}) | No cert infrastructure |
| Timeout | 300s create / 60s default | Same | Already implemented |
| Wire format | Hand-written JSON types | Generated protobuf | Proto exists; `make proto` target exists but not wired |

### 2.3 Client Tests

`packages/runtime-client/client_test.go` — 8 tests using `httptest.Server`. All tests prove
**HTTP transport contract only** — no gRPC, no mTLS, no real Host Agent. These tests prove
the client can correctly serialize/deserialize JSON and propagate HTTP errors. They do NOT test:
- Real gRPC streaming or deadline propagation
- mTLS handshake or certificate rotation
- Real Host Agent backend responses

---

## 3. Host Agent Runtime Files: Real vs Fake vs Dry-Run

### 3.1 File Inventory

| File | Type | Description |
|------|------|-------------|
| `service.go` | **Real** | RuntimeService — orchestrates VMRuntime + RootfsManager + NetworkManager |
| `vm_runtime.go` | **Real** | VMRuntime interface + InstanceSpec + RuntimeInfo types |
| `firecracker.go` | **Real** | FirecrackerManager — launches real firecracker processes; has `FIRECRACKER_DRY_RUN` escape hatch |
| `qemu.go` | **Real** | QemuManager — launches real QEMU/KVM processes for multi-disk support |
| `network.go` | **Real** | NetworkManager — TAP/iptrules via executor; has `NETWORK_DRY_RUN` escape hatch |
| `rootfs.go` | **Real** | RootfsManager — qemu-img CoW overlay materialization; has idempotent Materialize/Delete |
| `cloudinit.go` | **Real** | Cloud-init seed ISO generation (user-data, meta-data) |
| `console.go` | **Real** | Console logger — per-instance console.log files |
| `artifacts.go` | **Real** | ArtifactManager — instance directories, PID/socket/seed/console path management |
| `executor.go` | **Real** | Executor interface + RealExecutor — abstracted command execution for testability |
| `config.go` | **Real** | Host agent configuration |
| `rediscovery.go` | **Real** | PID file rediscovery for surviving host-agent restarts |
| `storage.go` | **Real** | LocalStorageManager — local block-volume allocation for volumes/snapshots |
| `network_inspect.go` | **Real** | Network state inspection (iptables rules dump) |
| `network_policy.go` | **Real** | Security group iptables policy enforcement |
| `runtime_fake.go` | **Fake** | FakeRuntime — in-memory test double; records calls, no real processes |
| `kvm_acceptance_test.go` | **Opt-in KVM tests** | Real QEMU lifecycle tests; disabled by default (`VM_PLATFORM_ENABLE_KVM_TESTS=1`) |

### 3.2 Dry-Run Escape Hatches

| Env Variable | File(s) | What It Skips |
|-------------|---------|---------------|
| `READINESS_DRY_RUN=true` | `worker/handlers/create.go:233`, `start.go:162`, `reboot.go:128` | SSH TCP dial readiness check |
| `NETWORK_DRY_RUN=true` | `host-agent/runtime/network.go:51` | All `ip(8)` and `iptables(8)` calls |
| `FIRECRACKER_DRY_RUN=true` | `host-agent/runtime/firecracker.go:87` | All `os.MkdirAll`, `exec.Command("firecracker")`, socket API calls; returns synthetic PID `999999999` |

**When any dry-run env is set, the affected code path is NOT production-proven.**
These are development escape hatches, not production modes.

### 3.3 Fake Implementations

| Implementation | File | Used By |
|---------------|------|---------|
| `FakeRuntime` | `runtime/runtime_fake.go` | Runtime unit tests, KVM acceptance tests (lifecycle logic) |
| `fakeStore` | `worker/handlers/create_test.go` | All worker handler unit tests |
| `fakeNetwork` | `worker/handlers/create_test.go` | All worker handler unit tests |
| `integFakeRuntime` | `test/integration/m2_vertical_slice_test.go` | Integration tests |
| `integFakeNetwork` | `test/integration/m2_vertical_slice_test.go` | Integration tests |

---

## 4. Tests That Prove Only Fake/Model Behavior

### 4.1 Worker Handler Tests (all use fake stores/networks/runtimes)

| File | Count | What It Tests | Framework |
|------|-------|---------------|-----------|
| `create_test.go` | 30+ tests | Create handler state machine, failure paths, rollback | `fakeStore` + `fakeNetwork` + `fakeRuntime` |
| `start_test.go` | 12+ tests | Start handler state machine, IP retention, fallback | `fakeStore` + `fakeStartRuntime` |
| `stop_test.go` | 11+ tests | Stop handler state machine, idempotency, IP retention | `fakeStore` + `fakeStopRuntime` |
| `reboot_test.go` | 8+ tests | Reboot handler state machine | `fakeStore` + `fakeRuntime` |
| `delete_test.go` (within create_test.go) | 5+ tests | Delete handler state machine, root disk disposition | `fakeStore` + `fakeRuntime` |
| `lifecycle_test.go` | Multiple | Full lifecycle sequences (create→stop→start→delete) | Same fake infra |
| `m6_ssh_nat_test.go` | 9 tests | SSH SLA contract + NAT lifecycle | Fake runtime + fake TCP listener |
| `volume_test.go` | Multiple | Volume create/attach/detach/delete | `fakeVolumeStore` |
| `snapshot_test.go` | Multiple | Snapshot create/delete/restore | `fakeSnapshotStore` |

**These tests prove correctness of the handler state machine, error propagation, and idempotency
logic — but none of them prove a real VM boots.**

### 4.2 Runtime Fake Tests

| File | What It Tests | Real Backend? |
|------|---------------|---------------|
| `runtime/runtime_test.go` | RuntimeService orchestration | **FakeRuntime** |
| `runtime/firecracker_dryrun_test.go` | FIRECRACKER_DRY_RUN mode | **Dry-run only** |
| `runtime/network_dryrun_test.go` | NETWORK_DRY_RUN mode | **Dry-run only** |
| `runtime/network_test.go` | Network manager with mock executor | **Mock executor** |
| `runtime/storage_test.go` | Local storage with temp dirs | **Temp dirs** (not real NFS) |
| `runtime/rootfs_test.go` | Rootfs manager idempotency | **Mock executor** (doesn't really run qemu-img) |
| `runtime/cloudinit_test.go` | Cloud-init seed generation | **File generation only** |

### 4.3 Integration Tests (fake runtimes but real DB)

| File | What It Tests | Runtime |
|------|---------------|---------|
| `test/integration/m2_vertical_slice_test.go` | Create→running→delete vertical slice | `integFakeRuntime` |
| `test/integration/m6_network_ssh_test.go` | IP allocation, release, uniqueness scan | `integFakeRuntime` + real DB |
| `test/integration/m6_ssh_nat_test.go` | SSH SLA + NAT lifecycle | Fake TCP listener |

---

## 5. Tests That Prove Linux Dataplane Behavior

### 5.1 KVM Acceptance Tests (`runtime/kvm_acceptance_test.go`)

**Status**: Opt-in, always skipped on macOS. Requires:
- `VM_PLATFORM_ENABLE_KVM_TESTS=1`
- Linux host with KVM
- `VM_PLATFORM_RUNTIME=qemu`
- `VM_PLATFORM_IMAGE_PATH=/path/to/ubuntu.qcow2` (optional — enables boot tests)
- `SSH_KEY_PATH=/path/to/id_ed25519.pub` (optional — enables SSH tests)

When fully enabled, these tests prove:
1. QEMU arg generation with cloud-init seed
2. Console log creation and population
3. Metadata service token flow (PUT /token → GET /metadata/v1/ssh-key)
4. QEMU process start/stop/reboot/delete lifecycle
5. Idempotent stop
6. Stop/start with same root disk
7. Delete cleans pid/socket/artifacts

**These are the ONLY tests in the repo that exercise a real hypervisor.**
They are NOT run in CI and are NOT run on macOS.

### 5.2 Network Privileged Tests (`runtime/network_privileged_test.go`)

**Status**: Requires Linux + `CAP_NET_ADMIN`. Tests actual `iptables` rule installation.
Not run in CI; not run on macOS.

### 5.3 Firecracker Production Path

There are **no automated tests** that exercise the production Firecracker path
(without `FIRECRACKER_DRY_RUN=true`). The real Firecracker path in `firecracker.go`
launches a real `firecracker` process and configures it via the Unix socket API —
this code is **deployed but never tested by automated suites**.

---

## 6. Tests Still Needed for Real Guest Boot

### 6.1 Critical Missing Tests

| Gap | What's Missing | Why It Matters |
|-----|---------------|----------------|
| **Real VM boot to SSH** | No test boots a real Firecracker/QEMU VM, waits for cloud-init, and verifies SSH | SSH readiness gating is the core RUNNING signal |
| **Real Firecracker lifecycle** | No test exercises `firecracker.go` production path (non-dry-run) | The primary production backend is untested |
| **Real network dataplane** | No test verifies TAP + NAT iptables rules on a Linux host | Instance networking is the primary value |
| **Real rootfs persistence** | No test verifies NFS qcow2 survives across stop/start/reboot cycles | Persistent root disk is a Phase 1 requirement |
| **Real cleanup on delete** | No test verifies TAP deletion, IP release, rootfs deletion on a Linux host | Resource leaks in production |
| **Reconciler with real runtime** | Reconciler tests use fake DB data; never queries a real host agent | Drift detection is only as good as the runtime query |
| **Real metadata service** | No test verifies a real VM can PUT /token → GET /metadata/v1/ssh-key via the metadata IP (169.254.169.254) | SSH key injection is critical for guest access |

### 6.2 Known Gaps Documented in Source

| Source | Gap |
|--------|-----|
| `client.go:8-13` | gRPC transport not yet implemented |
| `client.go:22-25` | mTLS auth not yet implemented |
| `firecracker.go:26` | Phase 1 uses direct execution, no jailer |
| `firecracker.go:530-531` | `Start()` not implemented on FirecrackerManager |
| `firecracker.go:540-541` | `Reboot()` not implemented on FirecrackerManager |
| `service.go:204-205` | `StartInstance` RPC is a stub ("This RPC is a stub for future use") |
| `service.go:261-275` | `ListInstances` doesn't honor passed context (uses Background) |
| `runtime.proto` (line 16) | "FROZEN at M0. Changes require formal review" |
| `m2-gate-check.sh:210-237` | Documents remaining gaps: gRPC protoc generation, mTLS, SSH key injection, cloud-init base image |
| M5_STATUS.md:109-115 | Linux/KVM hardware validation deferred; `idempotency_keys` table not fully wired |

---

## 7. Exact Recommended File Targets for Jobs 2–5

### Job 2 — Real Guest Boot Verification

**Target files:**
- `services/host-agent/runtime/firecracker.go:142-252` — Exercise `StartVM` without dry-run
- `services/host-agent/runtime/kvm_acceptance_test.go` — Enable real Firecracker tests
- `services/worker/handlers/create.go:233-240` — Remove or provide real `READINESS_DRY_RUN`
- `services/worker/handlers/m6_ssh_nat_test.go` — Add real TCP listener + cloud-init simulation

**Scope**: Make at least one test bootstrap a real Firecracker VM on a Linux host and verify the
TCP port 22 readiness signal.

### Job 3 — SSH Key Injection + Metadata Service

**Target files:**
- `services/host-agent/metadata/server.go` — Wire into host-agent main
- `services/host-agent/metadata/imdsv2.go` — Token store
- `services/worker/handlers/create.go:204-220` — Plumb `SSHPublicKey` through to CreateInstance
- `services/host-agent/runtime/service.go:197-206` — Implement StartInstance RPC

**Scope**: Ensure SSH keys from the API reach the guest via cloud-init / metadata service.

### Job 4 — Real Persistence Verification

**Target files:**
- `services/host-agent/runtime/rootfs.go` — Verify qcow2 survives stop/start cycles
- `services/worker/handlers/stop.go:100-108` — Verify DeleteRootDisk flag behavior
- `services/worker/handlers/start.go:134-159` — Verify rootfs path resolution after stop
- `services/host-agent/runtime/qemu.go` — QEMU multi-disk instances with real volumes

**Scope**: Prove persistent root disk semantics with real stop/start cycles and verify
delete_on_termination behavior.

### Job 5 — Reconciliation + Cleanup with Real Runtime

**Target files:**
- `services/reconciler/reconciler.go` — Wire real host-agent ListInstances query
- `services/reconciler/classifier.go` — Add `missing_runtime_process` detection using real query
- `services/host-agent/runtime/service.go:262-275` — Fix `ListInstances` context handling
- `services/reconciler/network_cleanup.go` — Real iptables cleanup verification

**Scope**: Prove the reconciler can detect and repair drift by querying real host agents.

---

## 8. Dry-Run / Mock / Fake Locations (Complete Index)

### Production code dry-run escape hatches:

| Location | Env Var | File:Line |
|----------|---------|-----------|
| SSH readiness skip | `READINESS_DRY_RUN` | `services/worker/handlers/create.go:233` |
| SSH readiness skip | `READINESS_DRY_RUN` | `services/worker/handlers/start.go:162` |
| SSH readiness skip | `READINESS_DRY_RUN` | `services/worker/handlers/reboot.go:128` |
| Network TAP/NAT skip | `NETWORK_DRY_RUN` | `services/host-agent/runtime/network.go:51` |
| Firecracker process skip | `FIRECRACKER_DRY_RUN` | `services/host-agent/runtime/firecracker.go:87` |
| Firecracker StartVM skip | `FIRECRACKER_DRY_RUN` | `services/host-agent/runtime/firecracker.go:147-153` |

### Test-only fake implementations:

| Fake Name | File | Purpose |
|-----------|------|---------|
| `FakeRuntime` | `services/host-agent/runtime/runtime_fake.go` | In-memory VMRuntime for unit tests |
| `fakeStore` | `services/worker/handlers/create_test.go:30` | In-memory InstanceStore |
| `fakeNetwork` | `services/worker/handlers/create_test.go:156` | In-memory NetworkController |
| `fakeRuntime` | `services/worker/handlers/create_test.go:136` | In-memory RuntimeClient |
| `fakeStopRuntime` | `services/worker/handlers/stop_test.go:22` | RuntimeClient with stop failure injection |
| `fakeStartRuntime` | `services/worker/handlers/start_test.go:23` | RuntimeClient with create failure injection |
| `integFakeRuntime` | `test/integration/m2_vertical_slice_test.go:57` | Integration test RuntimeClient |
| `integFakeNetwork` | `test/integration/m2_vertical_slice_test.go:45` | Integration test NetworkController |
| `fakeVolumeStore` | `services/worker/handlers/volume_test.go` | In-memory VolumeStore |
| `fakeSnapshotStore` | `services/worker/handlers/snapshot_test.go` | In-memory SnapshotStore |

---

## 9. Drift Between Markdown Docs and Code/Tests

### 9.1 M5_STATUS.md Drift

| Claim | Code Reality | Drift? |
|-------|-------------|--------|
| "Console UI (instance list, detail, create flow)" listed as M6 work | M6_STATUS.md line 247-249 explicitly says M5 was wrong — console is M7 | **Drift acknowledged in M6** |
| "Linux/KVM hardware validation deferred" | `kvm_acceptance_test.go` has opt-in KVM tests but only for QEMU, not Firecracker | **Accurate** |
| `idempotency_keys` table not fully wired | Code still uses sentinel jobs for idempotency, not the separate table | **Accurate** |

### 9.2 M6_STATUS.md Drift

| Claim | Code Reality | Drift? |
|-------|-------------|--------|
| "SSH login within 60 seconds of RUNNING state" | `waitForSSH` in create.go polls TCP/22. `READINESS_DRY_RUN=true` skips it | **Accurate but conditional** |
| "DNAT/SNAT rules correct across lifecycle states" | `network.go` implements idempotent iptables. `NETWORK_DRY_RUN=true` skips all rules | **Accurate but conditional** |
| "Live environment note: SSH end-to-end requires Linux/KVM" | `kvm_acceptance_test.go` exists but doesn't test Firecracker SSH | **Accurate** |

### 9.3 M4_STATUS.md Drift

| Claim | Code Reality | Drift? |
|-------|-------------|--------|
| Reconciler skeleton with drift classifier (5 classes) | `classifier.go` has all 5 classes. Tests use fake DB | **Accurate** |
| "Direct hypervisor query verification" is Phase 2 | Reconciler's `missing_runtime_process` classification checks `inst.UpdatedAt` > 5 min — never queries real hypervisor | **Accurate** |

### 9.4 README.md Drift

| Claim | Code Reality | Drift? |
|-------|-------------|--------|
| "host-agent seam" is present | Full VMRuntime + FirecrackerManager + QemuManager exist | **Accurate** |
| "SSH readiness gating" real | `waitForSSH` function exists but has dry-run escape hatch | **Real code but conditional** |
| "public-IP/NAT lifecycle handling" real | `network.go` has full ProgramNAT/RemoveNAT. Requires `NETWORK_DRY_RUN=false` on Linux | **Real code but platform-dependent** |

### 9.5 m2-gate-check.sh Drift

| Claim | Code Reality | Drift? |
|-------|-------------|--------|
| "INSTANCE_STOP handler — NOT YET IMPLEMENTED (M3 scope)" | `stop.go` is fully implemented with 11 tests. M6_STATUS.md also references it | **Script is stale** |
| "INSTANCE_START handler — NOT YET IMPLEMENTED (M3 scope)" | `start.go` is fully implemented with 12 tests | **Script is stale** |
| "INSTANCE_REBOOT handler — NOT YET IMPLEMENTED (M3 scope)" | `reboot.go` is fully implemented with 8 tests | **Script is stale** |
| "gRPC protoc generation... deferred" | Proto file exists; `make proto` target exists; protoc generation not yet run | **Still true** |
| "mTLS cert wiring... deferred" | `client.go` sends plaintext; no cert infrastructure in code | **Still true** |

---

## 10. Highest-Risk Runtime Gaps (Priority Order)

1. **No automated test proves a real VM boots** — Firecracker production path is untested in CI.
   KVM tests exist but are opt-in and disabled by default. Firecracker has no real-boot test.

2. **All worker handler tests use fake stores + fake runtimes** — The handler logic is well-tested
   but the boundary with real infrastructure (network, host agent, Firecracker) has zero
   automated end-to-end coverage.

3. **Three `DRY_RUN` env vars control critical paths** — `READINESS_DRY_RUN`, `NETWORK_DRY_RUN`,
   `FIRECRACKER_DRY_RUN` are development escape hatches with no safeguards against accidental
   use in production.

4. **Runtime client is HTTP/JSON, not gRPC** — The proto contract is defined but not wired.
   No protoc generation, no gRPC server, no mTLS. The "pure transport swap" assumption is
   untested.

5. **`StartInstance` RPC on host-agent is a stub** (`service.go:204-205`) — Returns success
   without doing anything. The worker's start handler calls `CreateInstance` directly instead,
   which works for Phase 1 but creates coupling between the worker's assumption of full
   re-provision and the host agent's missing `StartInstance` implementation.

6. **`ListInstances` ignores passed context** (`service.go:262`) — Uses `context.Background()`
   instead of the caller's context, breaking cancellation and deadline propagation.

7. **Reconciler never queries real host agents** — Drift detection is based on DB stale-timestamp
   checks. The `ListInstances` RPC exists but the reconciler doesn't call it.

8. **m2-gate-check.sh is stale** — Lists stop/start/reboot as "not yet implemented" even though
   they are fully implemented and tested.

---

## 11. Unresolved Blockers

1. **No Linux CI** — All automated tests run on macOS. Real Firecracker/QEMU/KVM tests require
   Linux. The hardware gate (H1-H4 in `m2-gate-check.sh`) must be run manually.

2. **No protoc toolchain in CI** — `make proto` requires `protoc` and `protoc-gen-go-grpc`.
   The generated code is not committed; the proto is frozen but not compiled.

3. **No test infrastructure for real VMs** — There is no test framework for launching real
   Firecracker VMs and waiting for SSH in CI. The `kvm_acceptance_test.go` framework is
   QEMU-only and not integrated with Firecracker.

4. **`READINESS_DRY_RUN` has no guard** — Nothing prevents setting this env var in production,
   which would skip SSH readiness gating and mark instances as RUNNING before SSH is available.

---

## 12. Build / Test Verification Results

### Build results (2026-05-02)

| Target | Result | Notes |
|--------|--------|-------|
| `go build ./services/resource-manager` | **PASS** | Clean build |
| `go build ./services/worker` | **FAIL** | Pre-existing: `services/host-agent/runtime` fails to compile (see below) |
| `go build ./services/host-agent` | **FAIL** | Pre-existing: `services/host-agent/runtime` fails to compile (see below) |

### Pre-existing compilation errors in `services/host-agent/runtime`

These errors exist before this job and are NOT caused by any changes made here:

1. **`network.go`** — missing `"strconv"` import (used at lines 467, 477 in `ApplySGPolicy`).
   The SG policy functions were consolidated from `security_group.go` into `network.go` but
   the `strconv` import was not carried over.

2. **`security_group.go`** — duplicate declarations of `SGRule`, `ApplySGPolicy`, `RemoveSGPolicy`,
   `sgChainName`, `ensureFilterJump`, `removeFilterJump`, `applySGRule`, and `min` —
   all already declared in `network.go`. This file should be deleted.

3. **`server.go`** — unused imports `google.golang.org/grpc/codes` and `google.golang.org/grpc/status`.
   Additionally references `runtimev1.RuntimeServiceServer` from generated proto code that
   has not been generated yet.

**Recommended fix** (not in scope for this job but noted for Job 2):
- Delete `security_group.go` (code was consolidated into `network.go`)
- Add `"strconv"` import to `network.go`
- Fix `server.go` unused imports (or comment out until gRPC generation is wired)

### Test results (2026-05-02)

| Package | Result | Notes |
|---------|--------|-------|
| `packages/idgen` | PASS | |
| `packages/observability` | PASS | |
| `packages/queue` | PASS | |
| `packages/runtime-client` | PASS | 8 tests; httptest-based transport tests only |
| `packages/state-machine` | PASS | |
| `internal/auth` | PASS | |
| `internal/db` | PASS | |
| `services/reconciler` | PASS | 50 tests (janitor, classifier, rate_limit, dispatcher, reconciler); all use fake DB |
| `services/worker` | PASS | Worker loop tests |
| `services/worker/handlers` | PASS | ~80 tests (create, start, stop, reboot, delete, lifecycle, SSH/NAT, volume, snapshot); all use fake stores and fake runtimes |
| `services/host-agent/runtime` | **FAIL (build)** | Pre-existing compilation errors |
| `packages/contracts/runtimev1` | No test files | Proto only, no generated code |

### Git tracking verification

| Path | Tracked files | Status |
|------|--------------|--------|
| `console/node_modules/*` | **0** | PASS — removed from tracking |
| `console/dist/*` | **0** | PASS — removed from tracking |

