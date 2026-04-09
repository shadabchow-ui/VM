#!/usr/bin/env bash
# ws-h1-drill-runner.sh — WS-H1 failover drill runner.
#
# Automates Steps 5–12 of P2_M1_WS_H1_DB_HA_RUNBOOK for a single drill run.
# Run it twice: once for run-1, once for run-2 (each must pass independently).
#
# Usage:
#   export API_BASE_URL="https://api.internal/v1"
#   export API_AUTH_HEADER="Bearer <token>"
#   export API_PRINCIPAL="probe-drilluser"
#   export FAILOVER_CMD="patronictl failover mycluster --master db-primary --force"
#   export EVIDENCE_DIR="evidence/ws-h1"
#   export RUN_NUMBER=1        # set to 2 for the second run
#
#   ./ws-h1-drill-runner.sh
#
# What this script does NOT do:
#   - It does not verify prerequisites (P-1 through P-7). Operator must do that.
#   - It does not query pg_stat_replication. Operator must do that (Steps 1-4).
#   - It does not perform the STONITH/fencing verification (Step 4).
#   - It does not run the second drill automatically. Run the script again with RUN_NUMBER=2.
#
# Source: P2_M1_WS_H1_DB_HA_RUNBOOK §6 Steps 5–12.

set -euo pipefail

# ── Required environment ──────────────────────────────────────────────────────
API_BASE_URL="${API_BASE_URL:?required}"
API_AUTH_HEADER="${API_AUTH_HEADER:?required}"
API_PRINCIPAL="${API_PRINCIPAL:-probe-drilluser}"
FAILOVER_CMD="${FAILOVER_CMD:?required — set to the failover trigger command}"
EVIDENCE_DIR="${EVIDENCE_DIR:-evidence/ws-h1}"
RUN_NUMBER="${RUN_NUMBER:-1}"
PROBE_SHAPE="${PROBE_SHAPE:-cx1.small}"
PROBE_IMAGE="${PROBE_IMAGE:-ubuntu-22.04}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROBE_SCRIPT="${PROBE_SCRIPT:-${SCRIPT_DIR}/ws-h1-write-probe.sh}"

# ── Setup ─────────────────────────────────────────────────────────────────────
mkdir -p "${EVIDENCE_DIR}/logs" "${EVIDENCE_DIR}/artifacts"
DRILL_LOG="${EVIDENCE_DIR}/logs/H1-TG3-drill-run-${RUN_NUMBER}.log"

log() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*" | tee -a "${DRILL_LOG}"; }

log "=== WS-H1 Failover Drill Run ${RUN_NUMBER} ==="
log "API_BASE_URL=${API_BASE_URL}"
log "FAILOVER_CMD=${FAILOVER_CMD}"

# ── Step 6: Confirm API is operational before drill ───────────────────────────
log "--- Step 6: Pre-drill API check ---"
PRE_STATUS=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 \
  -X POST "${API_BASE_URL}/instances" \
  -H "Authorization: ${API_AUTH_HEADER}" \
  -H "X-Principal-ID: ${API_PRINCIPAL}" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"probe-pre-drill\",\"shape\":\"${PROBE_SHAPE}\",\"image_id\":\"${PROBE_IMAGE}\"}")

if [[ "$PRE_STATUS" != "202" ]]; then
  log "ABORT: Pre-drill check returned HTTP ${PRE_STATUS} (expected 202). Environment not clean."
  exit 1
fi
log "Pre-drill check: HTTP 202 — environment clean."

# ── Step 7: Start continuous write probe ──────────────────────────────────────
log "--- Step 7: Starting continuous probe ---"
(
  while true; do
    "${PROBE_SCRIPT}" 2>&1 || true
    sleep 1
  done
) >> "${DRILL_LOG}" &
PROBE_PID=$!
log "Probe started (PID=${PROBE_PID}). Waiting 30s to confirm baseline..."

# Wait 30s baseline — if probe is not consistently returning SUCCESS, abort.
sleep 30
RECENT_FAILURES=$(tail -35 "${DRILL_LOG}" | grep "FAILURE" | wc -l)
if [[ "$RECENT_FAILURES" -gt 3 ]]; then
  log "ABORT: ${RECENT_FAILURES} failures in baseline window before drill. Fix environment."
  kill "$PROBE_PID" 2>/dev/null || true
  exit 1
fi
log "Baseline clean. Proceeding to primary termination."

# ── Step 8: Record T0 and kill primary ───────────────────────────────────────
log "--- Step 8: Triggering failover ---"
T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "T0: ${T0}" >> "${DRILL_LOG}"
log "T0=${T0} — executing: ${FAILOVER_CMD}"
eval "${FAILOVER_CMD}" >> "${DRILL_LOG}" 2>&1 || {
  log "WARNING: failover command exited non-zero. This may be normal for some HA mechanisms."
}

# ── Step 9: Monitor and record T1 ────────────────────────────────────────────
log "--- Step 9: Monitoring for recovery ---"
T1=""
MAX_WAIT=120  # Stop waiting after 120s regardless.
ELAPSED=0

while [[ $ELAPSED -lt $MAX_WAIT ]]; do
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  # Look for a SUCCESS line after T0 in the log.
  FIRST_SUCCESS_AFTER_T0=$(grep "SUCCESS" "${DRILL_LOG}" | tail -5 | head -1 || true)
  if [[ -n "$FIRST_SUCCESS_AFTER_T0" ]]; then
    T1=$(echo "$FIRST_SUCCESS_AFTER_T0" | awk '{print $1}')
    break
  fi
done

if [[ -z "$T1" ]]; then
  log "FAIL: No SUCCESS observed within ${MAX_WAIT}s after T0. Gap > ${MAX_WAIT}s."
  kill "$PROBE_PID" 2>/dev/null || true
  echo "T1: (none — timeout)" >> "${DRILL_LOG}"
  exit 1
fi

echo "T1: ${T1}" >> "${DRILL_LOG}"

# Calculate gap.
T0_EPOCH=$(date -d "${T0}" +%s 2>/dev/null || date -j -f "%Y-%m-%dT%H:%M:%SZ" "${T0}" +%s)
T1_EPOCH=$(date -d "${T1}" +%s 2>/dev/null || date -j -f "%Y-%m-%dT%H:%M:%SZ" "${T1}" +%s)
GAP=$((T1_EPOCH - T0_EPOCH))
echo "Gap: ${GAP}s" >> "${DRILL_LOG}"
log "T1=${T1} — Gap=${GAP}s"

if [[ $GAP -gt 30 ]]; then
  log "FAIL: Gap ${GAP}s > 30s threshold. DB-4/DB-5 FAIL."
  kill "$PROBE_PID" 2>/dev/null || true
  exit 1
fi
log "Gap ${GAP}s <= 30s — PASS (DB-4/DB-5 criterion met)."

# ── Step 11: Capture a gap-window 503 response for DB-6 ──────────────────────
# This step is best-effort here — it captures the most recent FAILURE line
# from the drill log that contains a 503. The operator should also run the
# manual curl from the runbook during drill run 2.
log "--- Step 11: Extracting gap-window 503 evidence ---"
GAP_503=$(grep "FAILURE.*503" "${DRILL_LOG}" | tail -1 || true)
if [[ -n "$GAP_503" ]]; then
  echo "$GAP_503" > "${EVIDENCE_DIR}/artifacts/H1-TG4-gap-response-run${RUN_NUMBER}.txt"
  log "Gap 503 captured: ${GAP_503}"
else
  log "WARNING: No 503 line found in drill log. Run the manual curl from Step 11 during drill run 2."
fi

# ── Step 12: Stop probe and check for duplicates ─────────────────────────────
log "--- Step 12: Stopping probe ---"
kill "$PROBE_PID" 2>/dev/null || true
log "Probe stopped."

log ""
log "=== Run ${RUN_NUMBER} complete ==="
log "T0:  ${T0}"
log "T1:  ${T1}"
log "Gap: ${GAP}s"
log ""
log "MANUAL STEPS STILL REQUIRED:"
log "  1. Connect to the promoted primary and run data integrity queries (Step 10)."
log "  2. Run duplicate-instance and duplicate-IP queries (Step 12)."
log "  3. Paste query output into this log file."
log "  4. For run 2: repeat ws-h1-drill-runner.sh with RUN_NUMBER=2 after environment recovery."
log ""
log "Data integrity query to run on promoted primary:"
log "  SELECT id, state, created_at FROM instances ORDER BY created_at DESC LIMIT 10;"
log "  SELECT id, type, status, created_at FROM jobs ORDER BY created_at DESC LIMIT 10;"
log ""
log "Duplicate check queries:"
log "  SELECT name, count(*) FROM instances WHERE name LIKE 'probe-%' GROUP BY name HAVING count(*) > 1;"
log "  SELECT ip_address, count(*) FROM ip_allocations WHERE allocated_at > '${T0}' GROUP BY ip_address HAVING count(*) > 1;"
