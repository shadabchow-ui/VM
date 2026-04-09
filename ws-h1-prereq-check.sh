#!/usr/bin/env bash
# ws-h1-prereq-check.sh — Verify all WS-H1 prerequisites before starting the drill.
#
# Checks P-3 (host count) and P-7 (probe script works).
# P-1, P-2, P-4, P-5, P-6 require human judgment — this script prompts for them.
#
# Usage:
#   export DATABASE_URL="postgres://superuser:pass@primary:5432/compute_platform"
#   export API_BASE_URL="https://api.internal/v1"
#   export API_AUTH_HEADER="Bearer <token>"
#   export API_PRINCIPAL="probe-drilluser"
#   ./ws-h1-prereq-check.sh
#
# Source: P2_M1_WS_H1_DB_HA_RUNBOOK §4.

set -euo pipefail

PASS=0
FAIL=0

check() {
  local name="$1"; local cmd="$2"
  if eval "$cmd" > /dev/null 2>&1; then
    echo "  [PASS] ${name}"
    PASS=$((PASS + 1))
  else
    echo "  [FAIL] ${name}"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== WS-H1 Prerequisite Check ==="
echo ""
echo "--- Automated checks ---"

# P-3: Host fleet operational
if [[ -n "${DATABASE_URL:-}" ]]; then
  HOST_COUNT=$(psql "${DATABASE_URL}" -tAc "SELECT count(*) FROM hosts WHERE status = 'REGISTERED'" 2>/dev/null || echo "0")
  if [[ "$HOST_COUNT" -gt 0 ]]; then
    echo "  [PASS] P-3: ${HOST_COUNT} registered host(s) found"
    PASS=$((PASS + 1))
  else
    echo "  [FAIL] P-3: No registered hosts (count=0)"
    FAIL=$((FAIL + 1))
  fi
else
  echo "  [SKIP] P-3: DATABASE_URL not set — run manual check:"
  echo "         psql <PRIMARY> -c \"SELECT count(*) FROM hosts WHERE status = 'REGISTERED'\""
fi

# P-7: Write probe works
PROBE_SCRIPT="${PROBE_SCRIPT:-./ws-h1-write-probe.sh}"
if [[ -f "$PROBE_SCRIPT" ]]; then
  PROBE_RESULT=$(API_BASE_URL="${API_BASE_URL:-}" API_AUTH_HEADER="${API_AUTH_HEADER:-}" \
    API_PRINCIPAL="${API_PRINCIPAL:-probe}" bash "$PROBE_SCRIPT" 2>&1 || true)
  if echo "$PROBE_RESULT" | grep -q "SUCCESS"; then
    echo "  [PASS] P-7: Write probe returns SUCCESS"
    PASS=$((PASS + 1))
  else
    echo "  [FAIL] P-7: Write probe did not return SUCCESS: ${PROBE_RESULT}"
    FAIL=$((FAIL + 1))
  fi
else
  echo "  [SKIP] P-7: ${PROBE_SCRIPT} not found — generate it first"
fi

echo ""
echo "--- Human-judgment checks (confirm manually) ---"
echo "  [ ] P-1: P2-M0 sign-off complete, all 7 contracts FROZEN"
echo "  [ ] P-2: Phase 1 M8 CI suite green — CI run ID: ___________"
echo "  [ ] P-4: Reconciler heartbeat confirmed on control plane"
echo "  [ ] P-5: Staging drill environment available"
echo "  [ ] P-6: DB superuser credentials confirmed for pg_stat_replication"
echo ""
echo "=== Result: ${PASS} automated check(s) passed, ${FAIL} failed ==="

if [[ $FAIL -gt 0 ]]; then
  echo "Resolve FAIL items before starting the drill."
  exit 1
fi
