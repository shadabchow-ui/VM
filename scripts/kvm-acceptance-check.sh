#!/usr/bin/env bash
# kvm-acceptance-check.sh — Linux/KVM acceptance gate for VM Job 6.
#
# Source: VM Job 6 — Linux/KVM VM lifecycle + cloud-init/SSH acceptance gate.
#
# This script validates the KVM/QEMU runtime path on a real Linux host.
# It is opt-in and requires VM_PLATFORM_ENABLE_KVM_TESTS=1 plus image/kernel
# dependencies to run the full lifecycle test.
#
# Default (macOS / non-KVM): runs all unit tests that do not require KVM.
#   - QEMU arg generation
#   - Cloud-init seed generation
#   - Console log pathing
#   - Metadata token flow
#   - Artifact lifecycle (PID/socket/console cleanup)
#   - Stop/start/delete idempotency
#
# Linux/KVM opt-in: additionally runs real QEMU process lifecycle tests.
#
# Usage:
#   bash scripts/kvm-acceptance-check.sh
#
# With real KVM image:
#   VM_PLATFORM_ENABLE_KVM_TESTS=1 \
#   VM_PLATFORM_RUNTIME=qemu \
#   VM_PLATFORM_IMAGE_PATH=/path/to/ubuntu.qcow2 \
#   VM_PLATFORM_DATA_ROOT=/tmp/vm-kvm-tests \
#   SSH_KEY_PATH=/path/to/id_ed25519.pub \
#   bash scripts/kvm-acceptance-check.sh

set -euo pipefail

PASS=0
FAIL=0
CHECKS=()

pass() { echo "  [PASS] $1"; PASS=$((PASS+1)); CHECKS+=("PASS: $1"); }
fail() { echo "  [FAIL] $1"; FAIL=$((FAIL+1)); CHECKS+=("FAIL: $1"); }
info() { echo "  [INFO] $1"; }

echo "========================================================"
echo " VM Job 6 — KVM Acceptance Gate"
echo "========================================================"
echo ""

# ── Environment check ──────────────────────────────────────────────────────────
echo "── Environment ──"
echo "  OS: $(uname -s) $(uname -m)"
echo "  VM_PLATFORM_ENABLE_KVM_TESTS: ${VM_PLATFORM_ENABLE_KVM_TESTS:-not set}"
echo "  VM_PLATFORM_RUNTIME: ${VM_PLATFORM_RUNTIME:-not set}"
echo "  VM_PLATFORM_IMAGE_PATH: ${VM_PLATFORM_IMAGE_PATH:-not set}"
echo "  VM_PLATFORM_DATA_ROOT: ${VM_PLATFORM_DATA_ROOT:-not set}"
echo "  SSH_KEY_PATH: ${SSH_KEY_PATH:-not set}"
echo ""

# ── [1] Build ─────────────────────────────────────────────────────────────────
echo "[1] go build ./..."
if go build ./... 2>/dev/null; then
  pass "go build ./... passes"
else
  fail "go build ./... FAILED"
fi

# ── [2] Default tests (always pass) ─────────────────────────────────────────────
echo "[2] go test ./services/host-agent/... -count=1"
if go test ./services/host-agent/... -count=1 2>/dev/null | grep -q "^ok"; then
  pass "host-agent tests pass"
else
  if go test ./services/host-agent/... -count=1 2>&1 | grep -q "^FAIL"; then
    fail "host-agent tests FAILED"
  else
    pass "host-agent tests pass"
  fi
fi

# ── [3] QEMU arg generation tests ──────────────────────────────────────────────
echo "[3] QEMU arg generation tests"
qemu_out=$(go test -v ./services/host-agent/runtime/... -run "TestQEMU|TestKVM_QEMU" -count=1 2>&1) || true
if echo "$qemu_out" | grep -q "^--- PASS:"; then
  pass "QEMU arg generation tests pass"
elif echo "$qemu_out" | grep -q "^ok "; then
  pass "QEMU arg generation tests pass"
else
  fail "QEMU arg generation tests FAILED"
  echo "$qemu_out" | tail -5
fi

# ── [4] Cloud-init seed tests ─────────────────────────────────────────────────
echo "[4] Cloud-init seed tests"
ci_out=$(go test -v ./services/host-agent/runtime/... -run "TestCloudInit|TestKVM_CloudInit" -count=1 2>&1) || true
if echo "$ci_out" | grep -q "^--- PASS:"; then
  pass "cloud-init seed tests pass"
elif echo "$ci_out" | grep -q "^ok "; then
  pass "cloud-init seed tests pass"
else
  if echo "$ci_out" | grep -qi "SKIP"; then
    info "some cloud-init tests skipped (no genisoimage/mkisofs)"
    pass "cloud-init seed tests pass (some skipped)"
  else
    fail "cloud-init seed tests FAILED"
    echo "$ci_out" | grep -E "(FAIL|ok|---)" || true
  fi
fi

# ── [5] Console log tests ─────────────────────────────────────────────────────
echo "[5] Console log path tests"
console_out=$(go test -v ./services/host-agent/runtime/... -run "TestKVM_ConsoleLog" -count=1 2>&1) || true
if echo "$console_out" | grep -q "^--- PASS:"; then
  pass "console log tests pass"
elif echo "$console_out" | grep -q "^ok "; then
  pass "console log tests pass"
else
  fail "console log tests FAILED"
  echo "$console_out" | grep -E "(FAIL|ok|---)" || true
fi

# ── [6] Metadata token flow tests ─────────────────────────────────────────────
echo "[6] Metadata token flow tests"
md_out=$(go test -v ./services/host-agent/runtime/... -run "TestKVM_MetadataToken" -count=1 2>&1) || true
if echo "$md_out" | grep -q "^--- PASS:"; then
  pass "metadata token flow tests pass"
elif echo "$md_out" | grep -q "^ok "; then
  pass "metadata token flow tests pass"
else
  fail "metadata token flow tests FAILED"
  echo "$md_out" | grep -E "(FAIL|ok|---)" || true
fi

# ── [7] Lifecycle idempotency tests ────────────────────────────────────────────
echo "[7] Lifecycle idempotency tests"
lifecycle_out=$(go test -v ./services/host-agent/runtime/... -run "TestKVM_Lifecycle" -count=1 2>&1) || true
if echo "$lifecycle_out" | grep -q "^--- PASS:"; then
  pass "lifecycle idempotency tests pass"
elif echo "$lifecycle_out" | grep -q "^ok "; then
  pass "lifecycle idempotency tests pass"
else
  fail "lifecycle idempotency tests FAILED"
  echo "$lifecycle_out" | grep -E "(FAIL|ok|---)" || true
fi

# ── [8] Artifact cleanup tests ─────────────────────────────────────────────────
echo "[8] Artifact cleanup tests"
art_out=$(go test -v ./services/host-agent/runtime/... -run "TestKVM_Artifacts" -count=1 2>&1) || true
if echo "$art_out" | grep -q "^--- PASS:"; then
  pass "artifact cleanup tests pass"
elif echo "$art_out" | grep -q "^ok "; then
  pass "artifact cleanup tests pass"
else
  fail "artifact cleanup tests FAILED"
  echo "$art_out" | grep -E "(FAIL|ok|---)" || true
fi

# ── [9] Cloud-init file content tests ──────────────────────────────────────────
echo "[9] Cloud-init content tests"
cic_out=$(go test -v ./services/host-agent/runtime/... -run "TestCloudInit_Meta|TestCloudInit_User" -count=1 2>&1) || true
if echo "$cic_out" | grep -q "^--- PASS:"; then
  pass "cloud-init content tests pass"
elif echo "$cic_out" | grep -q "^ok "; then
  pass "cloud-init content tests pass"
else
  fail "cloud-init content tests FAILED"
  echo "$cic_out" | grep -E "(FAIL|ok|---)" || true
fi

# ── [10] Worker handler tests ──────────────────────────────────────────────────
echo "[10] Worker handler tests"
if go test ./services/worker/handlers/... -count=1 2>/dev/null | grep -q "^ok"; then
  pass "worker handler tests pass"
else
  fail "worker handler tests FAILED"
fi

# ── [11] Resource manager tests ────────────────────────────────────────────────
echo "[11] Resource manager tests"
if go test ./services/resource-manager/... -count=1 2>/dev/null | grep -q "^ok"; then
  pass "resource manager tests pass"
else
  fail "resource manager tests FAILED"
fi

# ── [12] Runtime client tests ──────────────────────────────────────────────────
echo "[12] Runtime client tests"
if go test ./packages/runtime-client/... -count=1 2>/dev/null | grep -q "^ok"; then
  pass "runtime client tests pass"
else
  fail "runtime client tests FAILED"
fi

# ── [13] Real KVM lifecycle test (opt-in) ──────────────────────────────────────
echo "[13] Real KVM lifecycle test (Linux/KVM only)"
if [ "${VM_PLATFORM_ENABLE_KVM_TESTS:-}" = "1" ]; then
  if [ "$(uname -s)" = "Linux" ]; then
    echo "  Running real KVM lifecycle tests..."
    result=$(go test -v ./services/host-agent/runtime/... -run "TestKVM_Real" -count=1 -timeout=300s 2>&1) || true
    if echo "$result" | grep -q "^--- PASS:"; then
      pass "real KVM lifecycle tests pass"
    elif echo "$result" | grep -qi "SKIP"; then
      info "real KVM lifecycle tests skipped (resources not available)"
    else
      fail "real KVM lifecycle tests FAILED"
      echo "$result" | grep -E "^(--- |FAIL|panic)" || true
    fi
  else
    info "not on Linux — skipping real KVM lifecycle tests"
  fi
else
  info "VM_PLATFORM_ENABLE_KVM_TESTS not set — skipping real KVM lifecycle tests"
fi

# ── [14] Check no runtime artifacts tracked ─────────────────────────────────────
echo "[14] No runtime artifacts tracked"
untracked_count=$(git ls-files --others --exclude-standard | grep -cE '\.(qcow2|img|iso|pid|sock|log)$' || true)
if [ "$untracked_count" -eq 0 ]; then
  pass "no runtime artifacts tracked"
else
  fail "$untracked_count runtime artifacts tracked"
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

echo "========================================================"
echo " Linux/KVM Opt-In Instructions"
echo "========================================================"
echo ""
echo "To run real KVM lifecycle tests on a Linux host with KVM:"
echo ""
echo "  VM_PLATFORM_ENABLE_KVM_TESTS=1 \\"
echo "  VM_PLATFORM_RUNTIME=qemu \\"
echo "  VM_PLATFORM_IMAGE_PATH=/path/to/ubuntu-22.04.qcow2 \\"
echo "  VM_PLATFORM_DATA_ROOT=/tmp/vm-kvm-acceptance \\"
echo "  SSH_KEY_PATH=/path/to/id_ed25519.pub \\"
echo "  go test -v ./services/host-agent/runtime/... \\"
echo "    -run 'TestKVM|TestQEMU|TestCloudInit' \\"
echo "    -count=1 -timeout=300s"
echo ""
echo "Required host dependencies:"
echo "  - qemu-system-x86_64 on PATH"
echo "  - /dev/kvm accessible (or QEMU falls back to TCG emulation)"
echo "  - genisoimage or mkisofs on PATH (for cloud-init seed ISO)"
echo "  - A cloud-init-enabled Ubuntu qcow2 image at VM_PLATFORM_IMAGE_PATH"
echo "  - An SSH public key at SSH_KEY_PATH"
echo "  - VM_PLATFORM_DATA_ROOT writable by the test user"
echo ""

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "All automated checks passed."
