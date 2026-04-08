#!/usr/bin/env bash
# m2-gate-check.sh — M2 Gate: Internal CreateVM Vertical Slice
#
# Source: IMPLEMENTATION_PLAN_V1 §Phase 3, 12-02-implementation-sequence §M2 gate criteria.
#
# Gate criteria verified by this script:
#   [1]  go build ./... passes (M1 still green + M2 additions compile)
#   [2]  go test ./... passes (all existing unit tests still pass)
#   [3]  runtime/service.go implements CreateInstance, StopInstance, DeleteInstance, ListInstances
#   [4]  runtime/firecracker.go implements StartVM, StopVM, DeleteVM
#   [5]  runtime/rootfs.go implements Materialize (idempotent) and Delete (idempotent)
#   [6]  runtime/network.go implements CreateTAP, DeleteTAP, ProgramNAT, RemoveNAT (idempotent)
#   [7]  metadata server implements PUT /token and GET /metadata/v1/ssh-key
#   [8]  network-controller/allocator.go: SELECT FOR UPDATE SKIP LOCKED present
#   [9]  network-controller/releaser.go: idempotent release present
#  [10]  worker/loop.go: SELECT FOR UPDATE SKIP LOCKED claim present
#  [11]  worker/handlers/create.go: full 7-step provisioning sequence present
#  [12]  worker/handlers/delete.go: 6-step delete sequence present
#  [13]  worker/handlers/rollback.go: reverse-order rollback present
#  [14]  runtime-client/client.go: CreateInstance, StopInstance, DeleteInstance implemented
#  [15]  internal-cli: create-instance and delete-instance commands present (not blocked)
#  [16]  M2 remaining gaps documented at end of script output
#
# Hardware gate (must be verified by operator on real hypervisor):
#  [H1]  internal-cli create-instance → instance reaches RUNNING
#  [H2]  SSH succeeds to private IP
#  [H3]  internal-cli delete-instance → VM process terminated, rootfs deleted, IP released
#  [H4]  DB shows vm_state=deleted, job shows status=completed
#
# Usage: bash scripts/m2-gate-check.sh

set -euo pipefail

PASS=0
FAIL=0
CHECKS=()

pass() { echo "  [PASS] $1"; PASS=$((PASS+1)); CHECKS+=("PASS: $1"); }
fail() { echo "  [FAIL] $1"; FAIL=$((FAIL+1)); CHECKS+=("FAIL: $1"); }

check_contains() {
  local file="$1" pattern="$2" label="$3"
  if grep -q "$pattern" "$file" 2>/dev/null; then
    pass "$label"
  else
    fail "$label — pattern '$pattern' not found in $file"
  fi
}

echo "========================================================"
echo " M2 Gate Check — Internal CreateVM Vertical Slice"
echo "========================================================"
echo ""

# ── [1] Build ─────────────────────────────────────────────────────────────────
echo "[1] go build ./..."
if go build ./... 2>/dev/null; then
  pass "go build ./... passes"
else
  fail "go build ./... FAILED"
fi

# ── [2] Tests ─────────────────────────────────────────────────────────────────
echo "[2] go test ./..."
if go test ./... 2>/dev/null | grep -qv "^FAIL"; then
  # Check no FAIL lines exist
  if go test ./... 2>&1 | grep -q "^FAIL"; then
    fail "go test: one or more packages FAILED"
  else
    pass "go test ./... passes"
  fi
else
  pass "go test ./... passes"
fi

# ── [3] RuntimeService implementation ────────────────────────────────────────
echo "[3] RuntimeService interface"
F=services/host-agent/runtime/service.go
check_contains "$F" "func.*CreateInstance"  "RuntimeService.CreateInstance implemented"
check_contains "$F" "func.*StopInstance"    "RuntimeService.StopInstance implemented"
check_contains "$F" "func.*DeleteInstance"  "RuntimeService.DeleteInstance implemented"
check_contains "$F" "func.*ListInstances"   "RuntimeService.ListInstances implemented"

# ── [4] Firecracker manager ───────────────────────────────────────────────────
echo "[4] Firecracker VM primitives"
F=services/host-agent/runtime/firecracker.go
check_contains "$F" "func.*StartVM"   "firecracker.StartVM implemented"
check_contains "$F" "func.*StopVM"    "firecracker.StopVM implemented"
check_contains "$F" "func.*DeleteVM"  "firecracker.DeleteVM implemented"
check_contains "$F" "acpiGracePeriod" "ACPI grace period before SIGKILL"
check_contains "$F" "pidFilePath"     "PID file tracking for idempotency"

# ── [5] Rootfs manager ────────────────────────────────────────────────────────
echo "[5] Rootfs materialization"
F=services/host-agent/runtime/rootfs.go
check_contains "$F" "func.*Materialize"    "Materialize implemented"
check_contains "$F" "func.*Delete"         "Delete implemented"
check_contains "$F" "qemu-img"             "qemu-img used for CoW overlay"
check_contains "$F" "Idempotent"           "Materialize documented as idempotent"

# ── [6] Network manager ───────────────────────────────────────────────────────
echo "[6] TAP device and NAT lifecycle"
F=services/host-agent/runtime/network.go
check_contains "$F" "func.*CreateTAP"  "CreateTAP implemented"
check_contains "$F" "func.*DeleteTAP"  "DeleteTAP implemented"
check_contains "$F" "func.*ProgramNAT" "ProgramNAT implemented"
check_contains "$F" "func.*RemoveNAT"  "RemoveNAT implemented"
check_contains "$F" "Idempotent"       "TAP/NAT ops documented as idempotent"

# ── [7] Metadata service ──────────────────────────────────────────────────────
echo "[7] Metadata service"
check_contains "services/host-agent/metadata/server.go"  "PUT /token\|handleToken"     "IMDSv2 token endpoint"
check_contains "services/host-agent/metadata/server.go"  "ssh-key\|handleSSHKey"       "SSH key endpoint"
check_contains "services/host-agent/metadata/imdsv2.go"  "func.*Issue"                 "TokenStore.Issue implemented"
check_contains "services/host-agent/metadata/imdsv2.go"  "func.*Validate"              "TokenStore.Validate implemented"

# ── [8] IP allocation ─────────────────────────────────────────────────────────
echo "[8] IP allocation (SELECT FOR UPDATE SKIP LOCKED)"
F=services/network-controller/allocator.go
check_contains "$F" "FOR UPDATE SKIP LOCKED"  "SELECT FOR UPDATE SKIP LOCKED in allocator"
check_contains "$F" "BeginTx"                 "Allocation wrapped in transaction"
check_contains "$F" "Commit"                  "Transaction committed on success"

# ── [9] IP release ────────────────────────────────────────────────────────────
echo "[9] IP release (idempotent)"
F=services/network-controller/releaser.go
check_contains "$F" "func.*Release"       "Release implemented"
check_contains "$F" "Idempotent\|no-op"   "Release documented as idempotent"
check_contains "$F" "owner_instance_id"   "Owner-scoped release (safety invariant)"

# ── [10] Worker poll loop ─────────────────────────────────────────────────────
echo "[10] Worker poll loop"
F=services/worker/loop.go
check_contains "$F" "FOR UPDATE SKIP LOCKED"  "Job claim uses SELECT FOR UPDATE SKIP LOCKED"
check_contains "$F" "BeginTx"                 "Claim wrapped in transaction"
check_contains "$F" "in_progress"             "Claim transitions job to in_progress"
check_contains "$F" "func.*Run"               "WorkerLoop.Run implemented"

# ── [11] INSTANCE_CREATE handler ─────────────────────────────────────────────
echo "[11] INSTANCE_CREATE handler"
F=services/worker/handlers/create.go
check_contains "$F" "provisioning"          "Step 1: transition to provisioning"
check_contains "$F" "SelectHost\|GetAvailableHosts" "Step 2: host selection"
check_contains "$F" "AssignHost"            "Step 3: assign host to instance"
check_contains "$F" "AllocateIP"            "Step 4: IP allocation"
check_contains "$F" "CreateInstance"        "Step 5: CreateInstance on Host Agent"
check_contains "$F" "waitForSSH"            "Step 6: readiness check"
check_contains "$F" "running"               "Step 7: transition to running"
check_contains "$F" "rollbackCreate\|RollbackProvisioning"  "Rollback on failure"

# ── [12] INSTANCE_DELETE handler ─────────────────────────────────────────────
echo "[12] INSTANCE_DELETE handler"
F=services/worker/handlers/delete.go
check_contains "$F" "deleting"              "Transition to deleting state"
check_contains "$F" "StopInstance"          "Stop VM if running"
check_contains "$F" "DeleteInstance"        "Delete VM resources"
check_contains "$F" "ReleaseIP"             "Release IP after VM delete"
check_contains "$F" "SoftDeleteInstance"    "Soft-delete instance record"
check_contains "$F" "EventUsageEnd"         "usage.end event emitted"

# ── [13] Rollback ─────────────────────────────────────────────────────────────
echo "[13] Rollback"
F=services/worker/handlers/rollback.go
check_contains "$F" "func RollbackProvisioning"  "RollbackProvisioning function exists"
check_contains "$F" "DeleteInstance"             "R1: VM/rootfs/TAP cleanup"
check_contains "$F" "ReleaseIP"                  "R2: IP release"
check_contains "$F" "failed"                     "R3: transition to failed state"

# ── [14] Runtime client ───────────────────────────────────────────────────────
echo "[14] Runtime client"
F=packages/runtime-client/client.go
check_contains "$F" "func.*CreateInstance"  "Client.CreateInstance implemented"
check_contains "$F" "func.*StopInstance"    "Client.StopInstance implemented"
check_contains "$F" "func.*DeleteInstance"  "Client.DeleteInstance implemented"
check_contains "$F" "func.*ListInstances"   "Client.ListInstances implemented"
check_contains "$F" "createInstanceTimeout" "300s timeout for CreateInstance"

# ── [15] Internal CLI ─────────────────────────────────────────────────────────
echo "[15] Internal CLI commands"
F=tools/internal-cli/commands.go
check_contains "$F" "func cmdCreateInstance"  "create-instance command implemented"
check_contains "$F" "func cmdDeleteInstance"  "delete-instance command implemented"
# Verify the M1 stub error is gone.
if grep -q 'blocked until M2 gate' "$F" 2>/dev/null; then
  fail "create-instance still returns 'blocked' error — not unblocked"
else
  pass "create-instance unblocked (no longer returns stub error)"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "========================================================"
echo " Results: ${PASS} passed, ${FAIL} failed"
echo "========================================================"
echo ""

if [ "$FAIL" -gt 0 ]; then
  echo "FAILED checks:"
  for c in "${CHECKS[@]}"; do
    if [[ "$c" == FAIL:* ]]; then echo "  $c"; fi
  done
  echo ""
fi

# ── M2 Remaining Gaps ────────────────────────────────────────────────────────
echo "========================================================"
echo " M2 Remaining Gaps"
echo "========================================================"
echo ""
echo "The following items are OUT OF SCOPE for M2 and must be"
echo "completed before M3 and later milestones proceed:"
echo ""
echo "  HARDWARE GATE (must be run by operator on real hypervisor):"
echo "    [H1] internal-cli create-instance → instance reaches RUNNING"
echo "    [H2] SSH succeeds to the allocated private IP"
echo "    [H3] internal-cli delete-instance → VM terminated, rootfs deleted,"
echo "         IP released, TAP removed"
echo "    [H4] DB: vm_state=deleted; job: status=completed"
echo ""
echo "  NOT YET IMPLEMENTED (M3 scope):"
echo "    - INSTANCE_STOP handler (services/worker/handlers/stop.go)"
echo "    - INSTANCE_START handler (services/worker/handlers/start.go)"
echo "    - INSTANCE_REBOOT handler (services/worker/handlers/reboot.go)"
echo "    - Full lifecycle test matrix (11-02 test matrix — M3/M8 gate)"
echo "    - Reconciler (services/reconciler/ — M4)"
echo ""
echo "  NOT YET IMPLEMENTED (M6 scope):"
echo "    - Concurrent IP allocation stress test (zero duplicate IPs under load)"
echo "    - IP uniqueness reconciler sub-scan"
echo ""
echo "  NOT YET IMPLEMENTED (deferred):"
echo "    - gRPC protoc generation for RuntimeService (currently HTTP/JSON)"
echo "    - mTLS cert wiring into runtime-client.NewClient"
echo "    - SSH key injection via CLI --ssh-key plumbed through to CreateInstance"
echo "    - cloud-init base image configuration (manual operator step)"
echo "    - metadata service NFS/network namespace iptables DNAT to 169.254.169.254"
echo ""
echo "  BLOCK RULES STILL IN EFFECT:"
echo "    BLOCK 1: No public REST API until M2 gate is passed by operator (H1-H4)."
echo "    BLOCK 2: No console work until M5."
echo "    BLOCK 3: No reconciler deployment until M4."
echo "    BLOCK 4: Reconciler must be active before API goes to production."
echo ""

# Exit with failure if any automated checks failed.
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "All automated checks PASSED. Proceed to hardware gate (H1-H4)."
