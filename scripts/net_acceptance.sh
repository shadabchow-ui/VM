#!/usr/bin/env bash
# scripts/net_acceptance.sh — VM Platform Privileged Linux Networking Acceptance Gate.
#
# Runs opt-in privileged Linux networking tests that verify real kernel state:
#   - TAP creation, bridge attachment, deletion
#   - NAT DNAT/SNAT rule installation/removal
#   - Security-group default deny, established/related allow
#   - Explicit TCP/22 ingress allow
#   - Security-group policy removal
#   - Stale network cleanup detection
#
# Requirements:
#   - Linux host with root or CAP_NET_ADMIN
#   - ip(8) and iptables(8) installed
#   - Go toolchain available
#
# Usage:
#   sudo bash scripts/net_acceptance.sh
#   sudo VM_PLATFORM_ENABLE_NET_TESTS=1 bash scripts/net_acceptance.sh
#
# The script sets VM_PLATFORM_ENABLE_NET_TESTS=1 automatically.
# It installs cleanup traps and runs tests against real kernel state.
# All identifiers use "cpvm-nettest-*" / "cpvm-test-*" prefixes.
#
# Safety guarantees:
#   - No destructive host-wide iptables flushes (never iptables -F without chain name)
#   - Only removes rules/chains tagged with deterministic cpvm-test identifiers
#   - TAP devices cleaned up on exit via trap
#   - Cleanup runs even on script failure or Ctrl+C

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

export VM_PLATFORM_ENABLE_NET_TESTS=1

# ── Cleanup trap ──────────────────────────────────────────────────────────────

cleanup() {
  local exit_code=$?
  echo ""
  echo "── Cleaning up test network artifacts ──"

  # Remove any cpvm-test TAP devices.
  for dev in $(ip link show 2>/dev/null | awk -F': ' '/tap-cpvm-te/ {print $2}' || true); do
    echo "  Removing TAP device: $dev"
    ip link set "$dev" nomaster 2>/dev/null || true
    ip link delete "$dev" 2>/dev/null || true
  done

  # Remove cpvm-test nat rules by comment matching.
  for table in nat filter; do
    for chain in PREROUTING POSTROUTING INPUT OUTPUT FORWARD; do
      iptables -t "$table" -L "$chain" -n --line-numbers 2>/dev/null | \
        awk '/cpvm-test/ {print $1}' | sort -rn | \
        while read -r num; do
          echo "  Removing $table/$chain rule #$num"
          iptables -t "$table" -D "$chain" "$num" 2>/dev/null || true
        done
    done
  done

  # Flush and delete cpvm-test custom chains.
  for table in filter nat; do
    iptables -t "$table" -L -n 2>/dev/null | awk '/^Chain cpvm-test/ {print $2}' | \
      while read -r chain; do
        echo "  Flushing and deleting chain: $chain (table=$table)"
        iptables -t "$table" -F "$chain" 2>/dev/null || true
        iptables -t "$table" -X "$chain" 2>/dev/null || true
      done
  done

  echo "── Cleanup complete ──"
  exit "$exit_code"
}

trap cleanup EXIT INT TERM

# ── Pre-flight checks ─────────────────────────────────────────────────────────

echo ""
echo "════════════════════════════════════════════════════════════════════════"
echo "  VM Platform Privileged Linux Networking Acceptance Gate"
echo "════════════════════════════════════════════════════════════════════════"
echo ""

echo "── Pre-flight checks ──"

OS="$(uname -s)"
if [[ "$OS" != "Linux" ]]; then
  echo "  ❌ This script requires Linux (current: $OS)"
  exit 1
fi
echo "  ✅ OS: Linux"

if ! command -v ip &>/dev/null; then
  echo "  ❌ ip(8) not found"
  exit 1
fi
echo "  ✅ ip(8) found: $(which ip)"

if ! command -v iptables &>/dev/null; then
  echo "  ❌ iptables(8) not found"
  exit 1
fi
echo "  ✅ iptables(8) found: $(which iptables)"

if [[ $EUID -ne 0 ]]; then
  echo "  ❌ Script must run as root (EUID=0) or with CAP_NET_ADMIN"
  exit 1
fi
echo "  ✅ Running as root"

echo ""

# ── Pre-test cleanup ──────────────────────────────────────────────────────────

echo "── Pre-test cleanup (removing any leftover cpvm-test artifacts) ──"
for dev in $(ip link show 2>/dev/null | awk -F': ' '/tap-cpvm-te/ {print $2}' || true); do
  echo "  Removing leftover TAP: $dev"
  ip link set "$dev" nomaster 2>/dev/null || true
  ip link delete "$dev" 2>/dev/null || true
done
for table in filter nat; do
  for chain in PREROUTING POSTROUTING INPUT OUTPUT FORWARD; do
    iptables -t "$table" -L "$chain" -n --line-numbers 2>/dev/null | \
      awk '/cpvm-test/ {print $1}' | sort -rn | \
      while read -r num; do
        iptables -t "$table" -D "$chain" "$num" 2>/dev/null || true
      done
  done
  iptables -t "$table" -L -n 2>/dev/null | awk '/^Chain cpvm-test/ {print $2}' | \
    while read -r chain; do
      iptables -t "$table" -F "$chain" 2>/dev/null || true
      iptables -t "$table" -X "$chain" 2>/dev/null || true
    done
done
echo "  ✅ Pre-test cleanup complete"
echo ""

# ── Run acceptance tests ──────────────────────────────────────────────────────

echo "── Running privileged Linux networking acceptance tests ──"
echo ""

cd "$ROOT"

PASS=0
FAIL=0

run_tests() {
  local pattern="$1"
  local label="$2"
  echo "  ▶ $label"
  if VM_PLATFORM_ENABLE_NET_TESTS=1 go test -v -count=1 -timeout=60s \
    ./services/host-agent/runtime/... \
    -run "$pattern" 2>&1 | sed 's/^/       /'; then
    echo "  ✅ $label — PASS"
    ((PASS++)) || true
  else
    echo "  ❌ $label — FAIL"
    ((FAIL++)) || true
  fi
  echo ""
}

run_tests "TestPrivilegedNet_TAPCreateBridgeAttachDelete" "TAP: create, bridge attach, delete"
run_tests "TestPrivilegedNet_TAPCreateIdempotent" "TAP: idempotent create/delete"
run_tests "TestPrivilegedNet_NATDNATSNATAddRemove" "NAT: DNAT/SNAT add, remove"
run_tests "TestPrivilegedNet_NATIdempotent" "NAT: idempotent program/remove"
run_tests "TestPrivilegedNet_SGDefaultDenyEstablishedAllow" "SG: default deny + established/related"
run_tests "TestPrivilegedNet_SGExplicitTCP22IngressAllow" "SG: explicit tcp/22 ingress allow"
run_tests "TestPrivilegedNet_SGPolicyRemoval" "SG: policy apply → remove"
run_tests "TestPrivilegedNet_StaleNetworkCleanupDetection" "Cleanup: stale network detection"
run_tests "TestPrivilegedNet_CleanupDoesNotAffectUnrelatedRules" "Cleanup: does not affect unrelated rules"

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "════════════════════════════════════════════════════════════════════════"
echo "  Network Acceptance Gate Results"
echo "════════════════════════════════════════════════════════════════════════"
echo ""
TOTAL=$(( PASS + FAIL ))
echo "  Total: $TOTAL   Pass: $PASS   Fail: $FAIL"
echo ""

if [[ $FAIL -gt 0 ]]; then
  echo "  ❌ NET ACCEPTANCE GATE: FAIL"
  echo "     $FAIL test(s) did not pass."
  exit 1
else
  echo "  ✅ NET ACCEPTANCE GATE: PASS"
  echo "     All $PASS tests passed against real Linux kernel state."
  exit 0
fi
