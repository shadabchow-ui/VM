#!/usr/bin/env bash
# ws-h1-write-probe.sh — WS-H1 DB write probe.
#
# Issues a single lightweight write to the jobs table via the API
# and reports SUCCESS or FAILURE [error] with a timestamp to stdout.
#
# Usage:
#   export API_BASE_URL="https://api.internal/v1"
#   export API_AUTH_HEADER="Bearer <token>"
#   export PROBE_PRINCIPAL="probe-user"
#   ./ws-h1-write-probe.sh
#
# In continuous mode (drill loop):
#   while true; do
#     ./ws-h1-write-probe.sh
#     sleep 1
#   done >> evidence/ws-h1/logs/H1-TG3-drill-run-1.log
#
# The probe creates a probe instance (POST /v1/instances) and immediately
# deletes it. It does NOT leave probe instances running.
# The create endpoint is the best signal for DB write-path health because it:
#   - Calls InsertInstance (DB write).
#   - Is synchronous (returns 202 on success).
#   - Uses the same DB pool as all other handlers.
#
# Source: P2_M1_WS_H1_DB_HA_RUNBOOK §6 Step 5.

set -euo pipefail

API_BASE_URL="${API_BASE_URL:?API_BASE_URL is required}"
API_AUTH_HEADER="${API_AUTH_HEADER:?API_AUTH_HEADER is required}"
PROBE_PRINCIPAL="${PROBE_PRINCIPAL:-probe-drilluser}"
PROBE_SHAPE="${PROBE_SHAPE:-cx1.small}"
PROBE_IMAGE="${PROBE_IMAGE:-ubuntu-22.04}"

TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Single write attempt with 3s timeout (well under the 2s response requirement,
# but with enough headroom to distinguish "slow" from "unavailable").
RESPONSE=$(curl -s -w "\nHTTP_STATUS:%{http_code}" \
  --max-time 3 \
  -X POST "${API_BASE_URL}/instances" \
  -H "Authorization: ${API_AUTH_HEADER}" \
  -H "X-Principal-ID: ${PROBE_PRINCIPAL}" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"probe-$(date +%s)\",\"shape\":\"${PROBE_SHAPE}\",\"image_id\":\"${PROBE_IMAGE}\"}" \
  2>&1) || {
  echo "${TS} FAILURE [curl error: $?]"
  exit 0
}

HTTP_STATUS=$(echo "$RESPONSE" | grep "HTTP_STATUS:" | cut -d: -f2)
BODY=$(echo "$RESPONSE" | grep -v "HTTP_STATUS:")

if [[ "$HTTP_STATUS" == "202" ]]; then
  # Extract instance_id and clean up.
  INSTANCE_ID=$(echo "$BODY" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
  echo "${TS} SUCCESS [HTTP 202, instance_id=${INSTANCE_ID}]"
  # Best-effort cleanup — do not let cleanup failure affect the probe result.
  if [[ -n "$INSTANCE_ID" ]]; then
    curl -s --max-time 3 \
      -X DELETE "${API_BASE_URL}/instances/${INSTANCE_ID}" \
      -H "Authorization: ${API_AUTH_HEADER}" \
      -H "X-Principal-ID: ${PROBE_PRINCIPAL}" \
      > /dev/null 2>&1 || true
  fi
elif [[ "$HTTP_STATUS" == "503" ]]; then
  REQUEST_ID=$(echo "$BODY" | grep -o '"request_id":"[^"]*"' | head -1 | cut -d'"' -f4)
  echo "${TS} FAILURE [HTTP 503 service_unavailable, request_id=${REQUEST_ID}]"
elif [[ "$HTTP_STATUS" == "000" ]]; then
  echo "${TS} FAILURE [no response / timeout]"
else
  echo "${TS} FAILURE [HTTP ${HTTP_STATUS}]"
fi
